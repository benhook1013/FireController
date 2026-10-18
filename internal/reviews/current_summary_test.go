package reviews

import (
	"fmt"
	"net/url"
	"reflect"
	"testing"
)

// Compare the indexed path to the prior whole-history reduction using genuine
// HTTP archive responses. Cache values are also checked independently against
// those selected records; no successful fixture fabricates derived rows.
func assertCurrentSummaryReduction(t *testing.T, f *apiFixture, number int64) PullResponse {
	t.Helper()
	var pull PullResponse
	nativeRead(t, f, fmt.Sprintf("/pulls/%d", number), &pull)
	var attempts AttemptsResponse
	nativeRead(t, f, fmt.Sprintf("/pulls/%d/attempts?limit=100", number), &attempts)
	var runs RunsResponse
	nativeRead(t, f, fmt.Sprintf("/pulls/%d/runs?limit=100", number), &runs)
	if attempts.HasMore || runs.HasMore || attempts.SnapshotID != pull.SnapshotID || runs.SnapshotID != pull.SnapshotID {
		t.Fatal("bounded summary fixture selected inconsistent archives")
	}
	all := make([]Attempt, 0, len(attempts.Items))
	// Archive pages are newest-position first; the prior reducer consumes
	// original observation order and resolves equal instants by later position.
	for i := len(attempts.Items) - 1; i >= 0; i-- {
		all = append(all, Attempt(attempts.Items[i].AttemptSummary))
	}
	if got, want := pull.Pull.LatestAttempts, latestAttempts(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("indexed latest attempts differ from full reduction:\ngot%+v\nwant%+v", got, want)
	}
	var cached summaryTotals
	if err := f.store.DB().QueryRow(`SELECT run_count,attempt_count,incomplete_run_count FROM review_current_totals WHERE project_id=? AND pull_number=?`, f.project, number).Scan(&cached.runs, &cached.attempts, &cached.incomplete); err != nil {
		t.Fatal(err)
	}
	var incomplete int64
	for _, run := range runs.Items {
		if !run.Complete {
			incomplete++
		}
	}
	if cached != (summaryTotals{int64(len(runs.Items)), int64(len(attempts.Items)), incomplete}) || pull.Pull.RunCount != int64(len(runs.Items)) || pull.Pull.AttemptCount != int64(len(attempts.Items)) {
		t.Fatalf("cached / compact totals differ from retained records: %+v pull%+v", cached, pull.Pull)
	}
	wantRecent := runs.Items
	if len(wantRecent) > 3 {
		wantRecent = wantRecent[:3]
	}
	if !reflect.DeepEqual(pull.Pull.RecentRuns, wantRecent) {
		t.Fatalf("recent-run summary differs from retained run order: got%+v want%+v", pull.Pull.RecentRuns, wantRecent)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal("published derived indexes are inconsistent", err)
	}
	return pull
}

func currentSummaryAttempt(id, channel, start, finish string) Attempt {
	a := nativeAttempt(id, "active")
	a.Channel, a.StartedAt, a.FinishedAt = channel, start, finish
	return a
}

func TestCurrentSummaryNativeOrderingMatchesFullReduction(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	pull.Complete = true
	pull.Attempts = []Attempt{
		currentSummaryAttempt("cli-offset", "cli", "2026-10-01T03:00:00+13:00", ""),
		currentSummaryAttempt("cli-nanos", "cli", "2026-09-30T14:00:00.000000001Z", ""),
		currentSummaryAttempt("hosted-offset", "hosted", "2026-10-01T01:30:00+12:00", ""),
		currentSummaryAttempt("hosted-later", "hosted", "2026-09-30T14:00:00Z", ""),
		currentSummaryAttempt("unknown-first", "manual", "", ""),
		currentSummaryAttempt("source-collision", "manual", "", ""),
		currentSummaryAttempt("subagent-earlier-position", "subagent", "2026-10-01T03:00:00+13:00", ""),
		currentSummaryAttempt("subagent-later-position", "subagent", "2026-09-30T14:00:00Z", ""),
	}
	pull.Runs = []RunInput{{SourceRunID: "incomplete-import", Channel: "manual", HeadSHA: pull.HeadSHA, Scope: "bounded imported inventory", Outcome: "active"}}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "summary-import")
	initial := assertCurrentSummaryReduction(t, f, 1)
	for i, want := range []string{"hosted-later", "cli-nanos", "source-collision", "subagent-later-position"} {
		if initial.Pull.LatestAttempts[i].SourceAttemptID != want {
			t.Fatal("fixture did not distinguish offset/nanosecond/unknown/tie ordering", initial.Pull.LatestAttempts)
		}
	}
	old := currentSummaryAttempt("oldest-native", "cli", "2026-09-29T00:00:00Z", "")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &old})
	if got := assertCurrentSummaryReduction(t, f, 1); got.Pull.Complete {
		t.Fatal("native refresh ignored cached incomplete-run count")
	}
	old.FinishedAt = "2026-09-30T15:00:00.000000001Z"
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &old})
	if got := assertCurrentSummaryReduction(t, f, 1).Pull.LatestAttempts[1].SourceAttemptID; got != old.SourceAttemptID {
		t.Fatal("oldest position's new finish did not become latest", got)
	}
	// Active timestamps can be corrected without reopening a settled state. A
	// regressed top candidate must reveal the next indexed retained attempt.
	old.FinishedAt = "2026-09-30T13:00:00Z"
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &old})
	if got := assertCurrentSummaryReduction(t, f, 1).Pull.LatestAttempts[1].SourceAttemptID; got != "cli-nanos" {
		t.Fatal("corrected finish hid the previous latest candidate", got)
	}
	for _, candidate := range []Attempt{
		currentSummaryAttempt("source-collision", "manual", "", ""),
		currentSummaryAttempt("native-equal-instant", "hosted", "2026-10-01T03:00:00+13:00", ""),
		currentSummaryAttempt("native-subagent-nanos", "subagent", "2026-09-30T14:00:00.000000002Z", ""),
	} {
		mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &candidate})
		assertCurrentSummaryReduction(t, f, 1)
	}
	var collisions int
	if err := f.store.DB().QueryRow(`SELECT count(DISTINCT source) FROM review_current_attempt_order WHERE project_id=? AND source_attempt_id='source-collision'`, f.project).Scan(&collisions); err != nil || collisions != 2 {
		t.Fatal("mixed producers sharing an external identity collapsed", collisions, err)
	}
	run := nativeRun("native-complete-summary")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	assertCurrentSummaryReduction(t, f, 1)
}

func TestCurrentSummaryObservationOmissionReturnAndReorder(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	pull.Complete = true
	pull.Runs = []RunInput{{SourceRunID: "changing-completeness", Channel: "manual", HeadSHA: pull.HeadSHA, Scope: "retained inventory", Outcome: "active"}}
	pull.Attempts = []Attempt{currentSummaryAttempt("first-tie", "manual", "", ""), currentSummaryAttempt("second-tie", "manual", "", "")}
	other := nativePull(2)
	f.observe(t, reviewRequest(0, []PullInput{pull, other}), "summary-original")
	assertCurrentSummaryReduction(t, f, 1)
	pull.Runs[0].Complete, pull.Runs[0].Outcome = true, "completed"
	pull.Runs[0].StartedAt, pull.Runs[0].FinishedAt = "2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z"
	pull.Attempts[0], pull.Attempts[1] = pull.Attempts[1], pull.Attempts[0]
	f.observe(t, reviewRequest(1, []PullInput{other, pull}), "summary-reorder-complete")
	if after := assertCurrentSummaryReduction(t, f, 1); !after.Pull.Complete || after.Pull.LatestAttempts[0].SourceAttemptID != "first-tie" {
		t.Fatal("completeness counter / reordered same-time winner stale", after.Pull)
	}
	omission := reviewRequest(2, []PullInput{other})
	omission.Retired = []RetiredPull{{Number: 1, Reason: "defunct"}}
	f.observe(t, omission, "summary-omit")
	assertCurrentSummaryReduction(t, f, 2)
	var omitted int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_current_totals WHERE project_id=? AND pull_number=1)+(SELECT count(*) FROM review_current_attempt_order WHERE project_id=? AND pull_number=1)`, f.project, f.project).Scan(&omitted); err != nil || omitted != 0 {
		t.Fatal("omitted membership left stale derived rows", omitted, err)
	}
	f.observe(t, reviewRequest(3, []PullInput{pull, other}), "summary-return")
	assertCurrentSummaryReduction(t, f, 1)
}

func TestCurrentSummaryOffsetOutsideUTCYearRangeMatchesFullReduction(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	pull.Attempts = []Attempt{
		currentSummaryAttempt("last-utc-year", "hosted", "9999-12-31T23:59:59.999999999Z", ""),
		currentSummaryAttempt("offset-utc-year-10000", "hosted", "9999-12-31T23:59:59.999999999-23:59", ""),
		currentSummaryAttempt("offset-before-year-zero", "manual", "0000-01-01T00:00:00+23:59", ""),
		currentSummaryAttempt("unknown-zero-time", "manual", "", ""),
	}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "summary-extreme-offsets")
	initial := assertCurrentSummaryReduction(t, f, 1)
	if initial.Pull.LatestAttempts[0].SourceAttemptID != "offset-utc-year-10000" || initial.Pull.LatestAttempts[1].SourceAttemptID != "unknown-zero-time" {
		t.Fatal("extreme offsets changed established max(start,finish) semantics", initial.Pull.LatestAttempts)
	}
	a := currentSummaryAttempt("native-utc-year-10000-tie", "hosted", "9999-12-31T23:59:59.999999999-23:59", "")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
	assertCurrentSummaryReduction(t, f, 1)
}

func TestCurrentSummaryFailedPublicationPreservesCachesCurrentAndWire(t *testing.T) {
	f := newAPIFixture(t)
	a := currentSummaryAttempt("retained-active", "cli", "2026-01-01T00:00:00Z", "")
	first := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
	assertCurrentSummaryReduction(t, f, 1)
	var pull PullResponse
	beforeWire := nativeRead(t, f, "/pulls/1", &pull)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_summary_event BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'synthetic post-pointer failure'); END`); err != nil {
		t.Fatal(err)
	}
	// Includes both derived tables, every immutable body and current reference,
	// event, receipt and metadata, after the intentional trigger is installed.
	before := historyMigrationDatabaseDigest(t, f)
	a.FinishedAt = "2026-01-01T01:00:00Z"
	_, err := nativePublish(f, NativePublication{Pull: nativePull(1), Attempt: &a})
	nativeErrorCode(t, err, "storage_unavailable")
	if after := historyMigrationDatabaseDigest(t, f); after != before {
		t.Fatal("post-pointer failure leaked caches, staged versions or current references")
	}
	if afterWire := nativeRead(t, f, "/pulls/1", &pull); string(afterWire) != string(beforeWire) || pull.SnapshotID != first.ID {
		t.Fatal("failed publication changed old current API bytes")
	}
	assertCurrentSummaryReduction(t, f, 1)
}

func TestCurrentSummaryVersionFourBackfillPreservesHistoricalWireAndCursors(t *testing.T) {
	f, snapshots, id, _ := historyMigrationLegacyFixture(t)
	if err := f.store.EnsureSchema(t.Context(), "reviews", append(reviewSchemaMigrations(), historyMigrationSQL())...); err != nil {
		t.Fatal(err)
	}
	var queue QueueResponse
	nativeRead(t, f, "/queue?limit=1", &queue)
	if queue.Cursor == "" {
		t.Fatal("v4 fixture continuation missing")
	}
	routes := []string{"/queue?limit=1", "/queue?limit=1&cursor=" + url.QueryEscape(queue.Cursor), "/pulls/7?snapshot=" + snapshots[0], "/pulls/7?snapshot=" + snapshots[2], "/runs/" + id + "?snapshot=" + snapshots[0], "/pulls/7/attempts?snapshot=" + snapshots[0]}
	before := make([]historyMigrationWire, len(routes))
	for i, route := range routes {
		before[i] = historyMigrationRead(t, f, route)
		if before[i].Status != 200 {
			t.Fatal("v4 migration fixture read failed", route, before[i])
		}
	}
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "6" {
		t.Fatal("current-summary schema not installed", version, err)
	}
	for i, route := range routes {
		if got := historyMigrationRead(t, f, route); !reflect.DeepEqual(got, before[i]) {
			t.Fatal("current-index backfill changed stored historical JSON/cursor/header", route)
		}
	}
	assertCurrentSummaryReduction(t, f, 7)
	assertCurrentSummaryReduction(t, f, 8)
	beforeCurrent := historyMigrationDatabaseDigest(t, f)
	if err := EnsureSchema(t.Context(), f.store); err != nil || historyMigrationDatabaseDigest(t, f) != beforeCurrent {
		t.Fatal("already-current startup changed derived indexes", err)
	}
}

func TestCurrentSummaryIntegrityRejectsMissingOrIncorrectDerivedRows(t *testing.T) {
	for _, damage := range []string{
		`DELETE FROM review_current_totals`,
		`UPDATE review_current_totals SET attempt_count=attempt_count+1`,
		`DELETE FROM review_current_attempt_order`,
		`UPDATE review_current_attempt_order SET sort_key='2026-12-31T00:00:00.000000000Z'`,
	} {
		t.Run(damage, func(t *testing.T) {
			f := newAPIFixture(t)
			a := nativeAttempt("integrity-attempt", "active")
			mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
			if _, err := f.store.DB().Exec(damage); err != nil {
				t.Fatal(err)
			}
			before := historyMigrationDatabaseDigest(t, f)
			if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err == nil {
				t.Fatal("derived corruption accepted", damage)
			}
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("integrity inspection rewrote corrupt storage")
			}
		})
	}
}

func TestCurrentSummaryMaintenanceCoversProjectsAndKeysetBoundary(t *testing.T) {
	f := newAPIFixture(t)
	first := f.project
	for i, project := range []string{first, "z-summary-second-project"} {
		count := 100
		if i == 1 {
			count = 31
			if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES(?,'Second summary project',702,'owner/summary-second')`, project); err != nil {
				t.Fatal(err)
			}
		}
		pulls := make([]PullInput, count)
		for n := range pulls {
			pulls[n] = samplePull(int64(n+1), fmt.Sprintf("Summary page pull %d", n+1))
		}
		f.project = project
		request := reviewRequest(0, pulls)
		request.RepositoryID += int64(i)
		f.observe(t, request, "summary-pages")
	}
	f.project = first
	before := historyMigrationDatabaseDigest(t, f)
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil || historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("valid multi-project maintenance changed rows or refused", err)
	}
	// The first project has 100 keys. Check its tail, the last key of the
	// first 128-row page, and the tail beyond that page in the next project.
	for _, key := range []struct {
		project string
		number  int64
	}{{first, 100}, {"z-summary-second-project", 28}, {"z-summary-second-project", 31}} {
		if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET run_count=1 WHERE project_id=? AND number=?`, key.project, key.number); err != nil {
			t.Fatal(err)
		}
		damaged := historyMigrationDatabaseDigest(t, f)
		if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err == nil || historyMigrationDatabaseDigest(t, f) != damaged {
			t.Fatal("maintenance missed a current compact count or repaired it", key)
		}
		if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET run_count=0 WHERE project_id=? AND number=?`, key.project, key.number); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil || historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("fixture restoration lost valid multi-project state", err)
	}
}

func TestCurrentSummaryMigrationCompactCorruptionRollsBackAndRetries(t *testing.T) {
	for _, mode := range []string{"valid", "run-count", "attempt-count", "latest-attempts"} {
		t.Run(mode, func(t *testing.T) {
			f, snapshots, _, _ := historyMigrationLegacyFixture(t)
			historyMigrationApply(t, f) // Genuine v4 tables, no v5 derived schema.
			historical := historyMigrationRead(t, f, "/pulls/7?snapshot="+snapshots[0])
			const selected = ` FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE c.project_id=? AND c.number=7`
			var revision, runs, attempts int64
			var latest string
			if err := f.store.DB().QueryRow(`SELECT v.version_revision,v.run_count,v.attempt_count,v.latest_attempts_json`+selected, f.project).Scan(&revision, &runs, &attempts, &latest); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "run-count":
				_, err := f.store.DB().Exec(`UPDATE review_pull_versions SET run_count=run_count+1 WHERE project_id=? AND number=7 AND version_revision=?`, f.project, revision)
				if err != nil {
					t.Fatal(err)
				}
			case "attempt-count":
				_, err := f.store.DB().Exec(`UPDATE review_pull_versions SET attempt_count=attempt_count+1 WHERE project_id=? AND number=7 AND version_revision=?`, f.project, revision)
				if err != nil {
					t.Fatal(err)
				}
			case "latest-attempts":
				_, err := f.store.DB().Exec(`UPDATE review_pull_versions SET latest_attempts_json='[]' WHERE project_id=? AND number=7 AND version_revision=?`, f.project, revision)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := historyMigrationDatabaseDigest(t, f)
			err := EnsureSchema(t.Context(), f.store)
			if mode != "valid" {
				nativeErrorCode(t, err, "storage_unavailable")
				if historyMigrationDatabaseDigest(t, f) != before {
					t.Fatal("refused compact-summary migration changed v4 schema, metadata or retained rows")
				}
				// Undo only the test's deliberate corruption, then retry the same
				// migration. Production migration never repairs retained versions.
				if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET run_count=?,attempt_count=?,latest_attempts_json=? WHERE project_id=? AND number=7 AND version_revision=?`, runs, attempts, latest, f.project, revision); err != nil {
					t.Fatal(err)
				}
				err = EnsureSchema(t.Context(), f.store)
			}
			if err != nil {
				t.Fatal("valid compact migration or retry failed", err)
			}
			a := nativeAttempt("after-compact-migration", "active")
			mustNativePublish(t, f, NativePublication{Pull: nativePull(7), Attempt: &a})
			if got := historyMigrationRead(t, f, "/pulls/7?snapshot="+snapshots[0]); !reflect.DeepEqual(got, historical) {
				t.Fatal("migration or subsequent publication changed original historical response bytes")
			}
		})
	}
}
