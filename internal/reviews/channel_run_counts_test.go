package reviews

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func channelCountsPull(t *testing.T, f *apiFixture, snapshot string) PullResponse {
	t.Helper()
	var p PullResponse
	route := "/pulls/1"
	if snapshot != "" {
		route += "?snapshot=" + snapshot
	}
	nativeRead(t, f, route, &p)
	return p
}
func completedChannelPull() PullInput {
	p := nativePull(1)
	for i := 0; i < 24; i++ {
		p.Runs = append(p.Runs, RunInput{SourceRunID: fmt.Sprintf("channel-%02d", i), Channel: []string{"hosted", "cli", "subagent", "manual"}[i%4], Scope: "completed clean comparison", Outcome: "completed", Complete: true, Attributable: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z"})
	}
	return p
}
func TestChannelRunCountsExactBeyondPreviewAndReusedPullVersion(t *testing.T) {
	f := newAPIFixture(t)
	pull := completedChannelPull()
	req := reviewRequest(0, []PullInput{pull})
	first := historyObservationPost(t, f, req, "counts-original")
	initial := channelCountsPull(t, f, "")
	if initial.Pull.RunCounts != (ChannelRunCounts{Hosted: 6, CLI: 6, Subagent: 6, Manual: 6}) || len(initial.Pull.RecentRuns) != 3 {
		t.Fatal(initial)
	}
	var preview RunsResponse
	nativeRead(t, f, "/pulls/1/runs?limit=20", &preview)
	if len(preview.Items) != 20 || !preview.HasMore {
		t.Fatal("fixture must exceed mixed preview", preview)
	}
	updated := pull
	updated.Runs = append([]RunInput(nil), pull.Runs...)
	updated.Runs[0] = RunInput{SourceRunID: "older-replacement", Channel: "manual", Scope: "retained replacement", Outcome: "unknown"}
	second := historyObservationPost(t, f, reviewRequest(1, []PullInput{updated}), "counts-replaced")
	current := channelCountsPull(t, f, "")
	old := channelCountsPull(t, f, first.ID)
	if current.Pull.RunCounts != (ChannelRunCounts{Hosted: 5, CLI: 6, Subagent: 6, Manual: 6}) || old.Pull.RunCounts != initial.Pull.RunCounts || !reflect.DeepEqual(current.Pull.RecentRuns, initial.Pull.RecentRuns) || current.Pull.RunCount != initial.Pull.RunCount {
		t.Fatal("captured channel totals differ", old, current)
	}
	var versions int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_pull_versions WHERE project_id=? AND number=1`, f.project).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("fixture must reuse identical compact pull version", versions, err)
	}
	for _, route := range []string{"/queue"} {
		var queue QueueResponse
		nativeRead(t, f, route, &queue)
		if len(queue.Items) != 1 || queue.Items[0].RunCounts != current.Pull.RunCounts {
			t.Fatal("queue surface lost counts", route, queue)
		}
	}
	before := historyMigrationDatabaseDigest(t, f)
	if replay := historyObservationPost(t, f, req, "counts-original"); replay != first || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("original replay changed totals")
	}
	stale := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "counts-stale", reviewRequest(0, []PullInput{pull}))
	if stale.Code != http.StatusConflict || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("stale update changed totals", stale.Code)
	}
	run := nativeRun("counts-native")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	got := channelCountsPull(t, f, "")
	if got.Pull.RunCounts != (ChannelRunCounts{Hosted: 5, CLI: 7, Subagent: 6, Manual: 6}) || channelCountsPull(t, f, second.ID).Pull.RunCounts != current.Pull.RunCounts {
		t.Fatal("native append or pinned second snapshot differs", got)
	}
	assertCurrentSummaryReduction(t, f, 1)
}

func TestChannelRunCountsHistoricalQueryUsesPullMembershipIndex(t *testing.T) {
	f := newAPIFixture(t)
	rows, err := f.store.DB().Query(`EXPLAIN QUERY PLAN `+historicalChannelRunCountsSQL, f.project, 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if rows.Err() != nil || !strings.Contains(plan.String(), "SEARCH m USING INDEX review_run_memberships_page (project_id=? AND pull_number=?)") || strings.Contains(plan.String(), "SCAN m") || strings.Contains(plan.String(), "SCAN v") {
		t.Fatal("historical counts must search only selected PR memberships and point versions", plan.String(), rows.Err())
	}
}

func TestChannelRunCountsCorruptionAndPublicationRollback(t *testing.T) {
	f := newAPIFixture(t)
	historyObservationPost(t, f, reviewRequest(0, []PullInput{completedChannelPull()}), "counts-seed")
	if _, err := f.store.DB().Exec(`UPDATE review_current_totals SET hosted_run_count=hosted_run_count+1`); err != nil {
		t.Fatal(err)
	}
	response := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/1", f.readToken, "", nil)
	if response.Code != 500 {
		t.Fatal("mismatched cached total was exposed", response.Code)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err == nil {
		t.Fatal("maintenance accepted corrupt sum")
	}
	if _, err := f.store.DB().Exec(`UPDATE review_current_totals SET hosted_run_count=hosted_run_count-1; CREATE TRIGGER reject_count_event BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'synthetic counts failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := historyMigrationDatabaseDigest(t, f)
	run := nativeRun("counts-rolled-back")
	if _, err := nativePublish(f, NativePublication{Pull: nativePull(1), Run: &run}); err == nil || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("failed native publication leaked count delta", err)
	}
}

func TestChannelRunCountsSchemaEightMigrationAndRestart(t *testing.T) {
	// Preserve both rollback boundaries: the bridge forces a real cache rebuild,
	// while this successor also checks the channel-count migration UPDATE.
	for _, scenario := range []struct{ name, failureSQL string }{
		{"cache rebuild", `UPDATE review_current_totals SET run_count=run_count+1; CREATE TRIGGER reject_counts_backfill BEFORE INSERT ON review_current_totals BEGIN SELECT RAISE(ABORT,'synthetic count backfill failure'); END`},
		{"channel-count migration", `CREATE TRIGGER reject_counts_backfill BEFORE UPDATE ON review_current_totals BEGIN SELECT RAISE(ABORT,'synthetic count backfill failure'); END`},
	} {
		t.Run(scenario.name, func(t *testing.T) {

			f, snapshots, _, _ := historyMigrationLegacyFixture(t)
			migrations := append(reviewSchemaMigrations(), historyMigrationSQL(), CurrentSummarySchemaSQLV8(), CurrentRunOrderSchemaSQL(), pullMetadataMigrationSQL(), attemptReasonMigrationSQL())
			if err := f.store.EnsureSchemaWithBackfill(t.Context(), "reviews", migrations, backfillCurrentReviewIndexes); err != nil {
				t.Fatal(err)
			}
			var version string
			if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "8" {
				t.Fatal("genuine populated v8 required", version, err)
			}
			if _, err := f.store.DB().Exec(scenario.failureSQL); err != nil {
				t.Fatal(err)
			}
			before := historyMigrationDatabaseDigest(t, f)
			if err := EnsureSchema(t.Context(), f.store); err == nil || before != historyMigrationDatabaseDigest(t, f) {
				t.Fatal("failed upgrade did not roll back metadata, DDL, cache and history", err)
			}
			if _, err := f.store.DB().Exec(`DROP TRIGGER reject_counts_backfill`); err != nil {
				t.Fatal(err)
			}
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			var p PullResponse
			nativeRead(t, f, "/pulls/7?snapshot="+snapshots[0], &p)
			if !p.Pull.RunCounts.Valid(p.Pull.RunCount) || p.Pull.RunCounts.CLI != p.Pull.RunCount {
				t.Fatal("v8 historical facts lost", p)
			}
			if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
				t.Fatal(err)
			}
			before = historyMigrationDatabaseDigest(t, f)
			if err := EnsureSchema(t.Context(), f.store); err != nil || before != historyMigrationDatabaseDigest(t, f) {
				t.Fatal("current startup repeated backfill", err)
			}
		})
	}
}

func TestChannelRunCountsTransferAndObservationOmissionReturn(t *testing.T) {
	t.Run("native append", func(t *testing.T) {
		f := newAPIFixture(t)
		pull := completedChannelPull()
		first := historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "counts-initial")
		selected := nativePull(1)
		for _, run := range []RunInput{{SourceRunID: "extra-hosted", Channel: "hosted", Scope: "retained native", Outcome: "unknown"}, {SourceRunID: "extra-cli", Channel: "cli", Scope: "retained native", Outcome: "failed"}} {
			mustNativePublish(t, f, NativePublication{Pull: selected, Run: &run})
		}
		current := channelCountsPull(t, f, "")
		if current.Pull.RunCount != 26 || current.Pull.RunCounts != (ChannelRunCounts{Hosted: 6, CLI: 6, Subagent: 6, Manual: 6}) || channelCountsPull(t, f, first.ID).Pull.RunCounts != current.Pull.RunCounts {
			t.Fatal(current)
		}
		attempt := nativeAttempt("counts-metadata-only", "prepared")
		mustNativePublish(t, f, NativePublication{Pull: selected, Attempt: &attempt})
		if after := channelCountsPull(t, f, ""); after.Pull.RunCounts != current.Pull.RunCounts {
			t.Fatal("attempt-only update lost channel totals", after)
		}
		assertCurrentSummaryReduction(t, f, 1)
	})
	t.Run("complete omission return empty", func(t *testing.T) {
		f := newAPIFixture(t)
		pull := completedChannelPull()
		first := historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "counts-initial")
		reduced := pull
		reduced.Runs = append([]RunInput(nil), pull.Runs[4:]...)
		historyObservationPost(t, f, reviewRequest(1, []PullInput{reduced}), "counts-reduced")
		if got := channelCountsPull(t, f, ""); got.Pull.RunCounts != (ChannelRunCounts{Hosted: 5, CLI: 5, Subagent: 5, Manual: 5}) {
			t.Fatal(got)
		}
		historyObservationPost(t, f, reviewRequest(2, []PullInput{pull}), "counts-returned")
		if got := channelCountsPull(t, f, ""); got.Pull.RunCounts != (ChannelRunCounts{Hosted: 6, CLI: 6, Subagent: 6, Manual: 6}) {
			t.Fatal(got)
		}
		historyObservationPost(t, f, reviewRequest(3, []PullInput{nativePull(1)}), "counts-empty")
		if got := channelCountsPull(t, f, ""); got.Pull.RunCounts != (ChannelRunCounts{}) || got.Pull.RunCount != 0 {
			t.Fatal(got)
		}
		if got := channelCountsPull(t, f, first.ID); got.Pull.RunCounts != (ChannelRunCounts{Hosted: 6, CLI: 6, Subagent: 6, Manual: 6}) {
			t.Fatal("old counts followed current emptiness", got)
		}
		assertCurrentSummaryReduction(t, f, 1)
	})
}

func TestChannelRunCountsFailedCaptureAndCompletedZeroOutcomeUpdates(t *testing.T) {
	f := newAPIFixture(t)
	run := nativeRun("outcome-transition")
	run.Outcome, run.Complete, run.Finalized = "failed", false, false
	run.Found, run.Accepted, run.Routed = 0, 0, 0
	run.Findings = nil
	revision := int64(0)
	publish := func(key string) Receipt {
		t.Helper()
		pull := nativePull(1)
		pull.Runs = []RunInput{run}
		request := reviewRequest(revision, []PullInput{pull})
		request.Source = "outcome-producer"
		receipt := historyObservationPost(t, f, request, key)
		revision++
		return receipt
	}
	first := publish("initial-failed")
	firstBytes := nativeSnapshotBytes(t, f, first.ID)
	failed := assertCurrentSummaryReduction(t, f, 1)
	if failed.Pull.RunCount != 1 || failed.Pull.RunCounts != (ChannelRunCounts{}) {
		t.Fatal("failed empty capture counted as review", failed)
	}
	run.Outcome, run.Complete = "completed", true
	second := publish("completed-zero")
	secondBytes := nativeSnapshotBytes(t, f, second.ID)
	completed := assertCurrentSummaryReduction(t, f, 1)
	if completed.Pull.RunCount != 1 || completed.Pull.RunCounts != (ChannelRunCounts{CLI: 1}) || completed.Pull.RecentRuns[0].Found != 0 || completed.Pull.RecentRuns[0].Finalized {
		t.Fatal("completed clean unfinalized review lost", completed)
	}
	run.Outcome, run.Complete = "failed", false
	publish("back-to-failed")
	if reverted := assertCurrentSummaryReduction(t, f, 1); reverted.Pull.RunCount != 1 || reverted.Pull.RunCounts != (ChannelRunCounts{}) {
		t.Fatal("completed-to-failed delta retained credit", reverted)
	}
	run.Outcome, run.Complete = "completed", true
	publish("completed-again")
	if switched := assertCurrentSummaryReduction(t, f, 1); switched.Pull.RunCount != 1 || switched.Pull.RunCounts != (ChannelRunCounts{CLI: 1}) {
		t.Fatal("restored completed-outcome delta differs", switched)
	}
	if old := channelCountsPull(t, f, first.ID); old.Pull.RunCounts != (ChannelRunCounts{}) || nativeSnapshotBytes(t, f, first.ID) != firstBytes {
		t.Fatal("failed historical snapshot changed", old)
	}
	if old := channelCountsPull(t, f, second.ID); old.Pull.RunCounts != (ChannelRunCounts{CLI: 1}) || nativeSnapshotBytes(t, f, second.ID) != secondBytes {
		t.Fatal("completed historical snapshot changed", old)
	}
	// Even a structurally valid sum below retained history must match the exact
	// completed membership reduction at the maintenance/backup boundary.
	if _, err := f.store.DB().Exec("UPDATE review_current_totals SET cli_run_count=0"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err == nil {
		t.Fatal("incorrect lower aggregate accepted")
	}
	// Existing one-shot derived-cache backfill uses the same outcome predicate;
	// no schema upgrade or ordinary-read rebuild is introduced.
	if err := backfillChannelRunCounts(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	assertCurrentSummaryReduction(t, f, 1)
	if nativeSnapshotBytes(t, f, first.ID) != firstBytes || nativeSnapshotBytes(t, f, second.ID) != secondBytes {
		t.Fatal("cache correction rewrote retained history")
	}
}

func TestChannelRunCountsBoundedSubtotals(t *testing.T) {
	for _, tc := range []struct {
		counts ChannelRunCounts
		total  int64
		valid  bool
	}{
		{ChannelRunCounts{}, 2, true},
		{ChannelRunCounts{CLI: 1}, 2, true},
		{ChannelRunCounts{Hosted: 1, CLI: 1}, 2, true},
		{ChannelRunCounts{Hosted: 1, CLI: 2}, 2, false},
		{ChannelRunCounts{CLI: -1}, 2, false},
		{ChannelRunCounts{}, -1, false},
		{ChannelRunCounts{Hosted: 1 << 62, CLI: 1 << 62}, 1<<63 - 1, false},
	} {
		if tc.counts.Valid(tc.total) != tc.valid {
			t.Fatal(tc)
		}
	}
}

func TestChannelRunCountsLegacyElevenValidationAndAtomicTwelveUpgrade(t *testing.T) {
	f := newAPIFixture(t)
	pull := completedChannelPull()
	for i := 0; i < 4; i++ {
		pull.Runs[i].Outcome = "failed"
		pull.Runs[i].Complete = false
	}
	snapshot := historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "legacy-counts")
	retained := nativeSnapshotBytes(t, f, snapshot.ID)
	removeExternalHostedForLegacyFixture(t, f.store)
	// Ten has no DDL changes. Reproduce a nine image with its original
	// all-outcome derived counters, without editing any retained source record.
	if _, err := f.store.DB().Exec("UPDATE metadata SET value='9' WHERE key='schema:reviews'"); err != nil {
		t.Fatal(err)
	}
	if err := backfillChannelRunCounts(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	var cli int64
	if err := f.store.DB().QueryRow("SELECT cli_run_count FROM review_current_totals").Scan(&cli); err != nil || cli != 6 {
		t.Fatal("legacy cache fixture not populated", cli, err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal("intact eleven cache rejected", err)
	}
	if _, err := f.store.DB().Exec("CREATE TRIGGER refuse_completed_counts BEFORE UPDATE ON review_current_totals BEGIN SELECT RAISE(ABORT,'count migration rollback'); END"); err != nil {
		t.Fatal(err)
	}
	before := historyMigrationDatabaseDigest(t, f)
	if err := EnsureSchema(t.Context(), f.store); err == nil || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("failed count upgrade changed database", err)
	}
	if _, err := f.store.DB().Exec("DROP TRIGGER refuse_completed_counts"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := f.store.DB().QueryRow("SELECT value FROM metadata WHERE key='schema:reviews'").Scan(&version); err != nil || version != "11" {
		t.Fatal("semantic version not advanced", version, err)
	}
	got := assertCurrentSummaryReduction(t, f, 1)
	if got.Pull.RunCount != 24 || got.Pull.RunCounts != (ChannelRunCounts{Hosted: 5, CLI: 5, Subagent: 5, Manual: 5}) || nativeSnapshotBytes(t, f, snapshot.ID) != retained {
		t.Fatal("upgrade changed source history or failed to reduce completed reviews", got)
	}
	before = historyMigrationDatabaseDigest(t, f)
	if err := EnsureSchema(t.Context(), f.store); err != nil || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("current startup rebuilt unchanged caches", err)
	}
	for _, legacy := range []int{9} {
		if channelRunCountAggregateForVersion(legacy) != legacyChannelRunCountAggregate {
			t.Fatal("legacy cache meaning changed", legacy)
		}
	}
}
