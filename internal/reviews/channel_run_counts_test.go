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
func mixedChannelPull() PullInput {
	p := nativePull(1)
	for i := 0; i < 24; i++ {
		p.Runs = append(p.Runs, RunInput{SourceRunID: fmt.Sprintf("channel-%02d", i), Channel: []string{"hosted", "cli", "subagent", "manual"}[i%4], Scope: "retained mixed outcome", Outcome: []string{"unknown", "active", "failed", "cancelled"}[i%4]})
	}
	return p
}
func TestChannelRunCountsExactBeyondPreviewAndReusedPullVersion(t *testing.T) {
	f := newAPIFixture(t)
	pull := mixedChannelPull()
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
	if current.Pull.RunCounts != (ChannelRunCounts{Hosted: 5, CLI: 6, Subagent: 6, Manual: 7}) || old.Pull.RunCounts != initial.Pull.RunCounts || !reflect.DeepEqual(current.Pull.RecentRuns, initial.Pull.RecentRuns) || current.Pull.RunCount != initial.Pull.RunCount {
		t.Fatal("captured channel totals differ", old, current)
	}
	var versions int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_pull_versions WHERE project_id=? AND number=1`, f.project).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("fixture must reuse identical compact pull version", versions, err)
	}
	for _, route := range []string{"/queue", "/queue?view=recent"} {
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
	if got.Pull.RunCounts != (ChannelRunCounts{Hosted: 5, CLI: 7, Subagent: 6, Manual: 7}) || channelCountsPull(t, f, second.ID).Pull.RunCounts != current.Pull.RunCounts {
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
	historyObservationPost(t, f, reviewRequest(0, []PullInput{mixedChannelPull()}), "counts-seed")
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
	f, snapshots, _, _ := historyMigrationLegacyFixture(t)
	migrations := append(reviewSchemaMigrations(), historyMigrationSQL(), CurrentSummarySchemaSQLV8(), CurrentRunOrderSchemaSQL(), pullMetadataMigrationSQL(), attemptReasonMigrationSQL())
	if err := f.store.EnsureSchemaWithBackfill(t.Context(), "reviews", migrations, backfillCurrentReviewIndexes); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "8" {
		t.Fatal("genuine populated v8 required", version, err)
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_counts_backfill BEFORE INSERT ON review_current_totals BEGIN SELECT RAISE(ABORT,'synthetic count backfill failure'); END`); err != nil {
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
}

func TestChannelRunCountsTransferAndObservationOmissionReturn(t *testing.T) {
	t.Run("additive", func(t *testing.T) {
		f := newAPIFixture(t)
		pull := mixedChannelPull()
		first := historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "counts-initial")
		selected := nativePull(1)
		selected.Runs = []RunInput{{SourceRunID: "extra-hosted", Channel: "hosted", Scope: "retained transfer", Outcome: "unknown"}, {SourceRunID: "extra-cli", Channel: "cli", Scope: "retained transfer", Outcome: "failed"}}
		_, apply, body := transferApplyFor(t, f, transferRequest(1, TransferGroup{Source: "extra-original-producer", Pulls: []PullInput{selected}}), "counts-additive")
		current := channelCountsPull(t, f, "")
		if current.Pull.RunCounts != (ChannelRunCounts{Hosted: 7, CLI: 7, Subagent: 6, Manual: 6}) || channelCountsPull(t, f, first.ID).Pull.RunCounts != (ChannelRunCounts{Hosted: 6, CLI: 6, Subagent: 6, Manual: 6}) {
			t.Fatal(current)
		}
		selected.Runs = nil
		transferApplyFor(t, f, transferRequest(2, TransferGroup{Source: "extra-original-producer", Pulls: []PullInput{selected}}), "counts-metadata-only")
		if after := channelCountsPull(t, f, ""); after.Pull.RunCounts != current.Pull.RunCounts {
			t.Fatal("omitted additive history lost channel totals", after)
		}
		before := historyMigrationDatabaseDigest(t, f)
		replayed := importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "counts-additive", importJSON(t, apply))
		if replayed.Code != 201 || replayed.Body.String() != body || before != historyMigrationDatabaseDigest(t, f) {
			t.Fatal("transfer replay changed counts", replayed.Code)
		}
		assertCurrentSummaryReduction(t, f, 1)
	})
	t.Run("complete omission return empty", func(t *testing.T) {
		f := newAPIFixture(t)
		pull := mixedChannelPull()
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
