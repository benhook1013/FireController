package reviews

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// Exercise the actual observation handler and its ordinary write deadline at
// the admitted aggregate bounds, spread over every allowed queue position.
func TestCurrentSummaryBulkObservationReplacement(t *testing.T) {
	f := newAPIFixture(t)
	queue := make([]PullInput, 100)
	for i := range queue {
		queue[i] = nativePull(int64(i + 1))
		for j := 0; j < 2; j++ {
			id := fmt.Sprintf("bulk-%d-%d", i, j)
			queue[i].Runs = append(queue[i].Runs, RunInput{SourceRunID: id, Channel: "cli", Scope: "bounded bulk observation", Outcome: "active"})
			queue[i].Attempts = append(queue[i].Attempts, nativeAttempt(id, "active"))
		}
	}
	initial := reviewRequest(0, queue)
	started := time.Now()
	first := historyObservationPost(t, f, initial, "bulk-original")
	t.Logf("200 runs/200 attempts/100 PRs initial actual observation: %s", time.Since(started))
	var pinned RunDetailResponse
	path := "/runs/" + runOpaqueID(f.project, initial.Source, queue[0].Runs[0].SourceRunID) + "?snapshot=" + first.ID
	original := nativeRead(t, f, path, &pinned)
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}

	// Copy the modified slices so the initial request remains an exact retry.
	updated := append([]PullInput(nil), queue...)
	for i := range updated {
		updated[i].Runs = append([]RunInput(nil), updated[i].Runs...)
		updated[i].Runs[0].Complete, updated[i].Runs[0].Outcome = true, "completed"
		updated[i].Runs[0].StartedAt, updated[i].Runs[0].FinishedAt = "2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z"
		updated[i].Title += " updated"
	}
	started = time.Now()
	historyObservationPost(t, f, reviewRequest(1, updated), "bulk-update")
	t.Logf("200-record update actual observation: %s", time.Since(started))
	for _, number := range []int64{1, 50, 100} {
		assertCurrentSummaryReduction(t, f, number)
	}

	// Failure after pointer/cache installation must roll the full replacement
	// back, including removals and their derived-row cascades.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_bulk_event BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'synthetic bulk failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := historyMigrationDatabaseDigest(t, f)
	reduced := reviewRequest(2, updated[:50])
	for _, pull := range updated[50:] {
		reduced.Retired = append(reduced.Retired, RetiredPull{Number: pull.Number, Reason: "defunct"})
	}
	response := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "bulk-rejected", reduced)
	if response.Code != http.StatusInternalServerError || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("failed full replacement changed retained rows", response.Code, response.Body.String())
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER reject_bulk_event`); err != nil {
		t.Fatal(err)
	}
	historyObservationPost(t, f, reduced, "bulk-omitted")
	var totals, orders int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_current_totals WHERE project_id=?),(SELECT count(*) FROM review_current_attempt_order WHERE project_id=?)`, f.project, f.project).Scan(&totals, &orders); err != nil || totals != 50 || orders != 100 {
		t.Fatal("omitted membership retained derived rows", totals, orders, err)
	}
	historyObservationPost(t, f, reviewRequest(3, updated), "bulk-return")
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	beforeReplay := historyMigrationDatabaseDigest(t, f)
	if replay := historyObservationPost(t, f, initial, "bulk-original"); replay != first || beforeReplay != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("exact original replay changed retained state")
	}
	if got := nativeRead(t, f, path, &pinned); string(got) != string(original) {
		t.Fatal("bulk replacement changed pinned run bytes")
	}
	empty := reviewRequest(4, nil)
	for _, pull := range updated {
		empty.Retired = append(empty.Retired, RetiredPull{Number: pull.Number, Reason: "defunct"})
	}
	historyObservationPost(t, f, empty, "bulk-empty")
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal("empty complete replacement left invalid derived rows", err)
	}
}

func TestCurrentSummaryBulkDeltaPlanMatchesReplacement(t *testing.T) {
	f := newAPIFixture(t)
	one, two, three := nativePull(1), nativePull(2), nativePull(3)
	one.Runs = []RunInput{{SourceRunID: "changing", Channel: "cli", Scope: "plan", Outcome: "active"}, {SourceRunID: "omitted", Channel: "cli", Scope: "plan", Outcome: "active"}}
	one.Attempts = []Attempt{nativeAttempt("keep-attempt", "active"), nativeAttempt("omit-attempt", "active")}
	two.Runs = []RunInput{{SourceRunID: "omitted-pull", Channel: "cli", Scope: "plan", Outcome: "active"}}
	two.Attempts = []Attempt{nativeAttempt("omitted-pull", "active")}
	f.observe(t, reviewRequest(0, []PullInput{one, two, three}), "plan-original")
	one.Runs = append([]RunInput(nil), one.Runs[:1]...)
	one.Runs[0].Complete, one.Runs[0].Outcome = true, "completed"
	one.Runs[0].StartedAt, one.Runs[0].FinishedAt = "2026-01-01T00:00:00Z", "2026-01-01T00:01:00Z"
	one.Attempts = append([]Attempt(nil), one.Attempts[:1]...)
	three.Runs = []RunInput{{SourceRunID: "new", Channel: "cli", Scope: "plan", Outcome: "active"}}
	three.Attempts = []Attempt{nativeAttempt("new", "active")}
	req := reviewRequest(1, []PullInput{one, three, nativePull(4)})
	if err := validateObservation(&req); err != nil {
		t.Fatal(err)
	}
	tx := historyWriterTx(t, f)
	snapshot := historyWriterSnapshot(t, tx, f.project, 2)
	write := newHistorySnapshotWrite(f.project, 2)
	if err := write.observation(t.Context(), tx, req); err != nil {
		t.Fatal(err)
	}
	want := write.replacementTotals
	write.replacementTotals = nil // Exercise the native touched-record plan.
	got, err := write.prepareCurrentTotals(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if got[2] != (summaryTotals{}) {
		t.Fatal("removed PR did not lose all records", got)
	}
	delete(got, 2)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("touched-record plan differs from complete input: got%+v want%+v", got, want)
	}
	var oldRuns int
	if err := tx.QueryRow(`SELECT count(*) FROM review_current_runs WHERE project_id=? AND pull_number=1`, f.project).Scan(&oldRuns); err != nil || oldRuns != 2 {
		t.Fatal("planning replaced old current references", oldRuns, err)
	}
	if err := write.applyCurrent(t.Context(), tx, snapshot, 1); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSummaries(t.Context(), tx); err != nil {
		t.Fatal("delta installation differs from actual current membership", err)
	}
}
