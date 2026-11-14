package reviewexec

import (
	"bytes"
	"strings"
	"testing"
)

func TestCoverageIndexRemovalRetainsJournalAndReplayAcrossRestart(t *testing.T) {
	f, a, _ := cliAllowanceFixture(t)
	if got := attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='review_operations_coverage'`); got != 0 {
		t.Fatal("fresh database maintains an unused coverage index")
	}
	// Reconstruct the earlier unused index in this owned fixture. Its
	// removal must preserve execution history and the original keyed receipt.
	admissionSQL(t, f, `CREATE INDEX review_operations_coverage ON review_operations(project_id,pull_number,channel,head_sha,state,id)`)
	var beforeVersion string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	restorePreRetryIndexSchema(t, f)
	admissionSQL(t, f, `DROP INDEX review_operations_listing`)
	admissionSQL(t, f, `DELETE FROM metadata WHERE key='schema:review_execution_indexes'`)
	journal := attemptState(t, f, a)
	var original []byte
	if err := f.store.DB().QueryRow(`SELECT response_json FROM receipts WHERE operation='reviews.request' LIMIT 1`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	cliAllowanceReopen(t, f)
	assertRetryIndexesRestored(t, f)
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != beforeVersion {
		t.Fatal("index migration renumbered execution-table migrations", version, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='review_operations_coverage'`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='review_operations_completed_comparison'`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='review_operations_cli_admission'`) != 0 || attemptState(t, f, a) != journal {
		t.Fatal("index migration changed retained journal or active scan index")
	}
	var retained []byte
	if err := f.store.DB().QueryRow(`SELECT response_json FROM receipts WHERE operation='reviews.request' LIMIT 1`).Scan(&retained); err != nil || !bytes.Equal(original, retained) {
		t.Fatal("index migration changed keyed response", err)
	}
	replay, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "allowance-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil || !bytes.Equal(original, replay) || attemptState(t, f, a) != journal {
		t.Fatal("reopened service did not replay the original request without effects", err)
	}
}

// Keep the independent index domain consistent when a migration fixture
// reconstructs a schema predating the retry indexes.
func restorePreRetryIndexSchema(t *testing.T, f *attemptFixture) {
	t.Helper()
	for _, index := range []string{"review_waits_live_retry", "review_transitions_exhaustion_time", "review_transitions_exhaustion_attempt", "review_operations_draft_probe"} {
		admissionSQL(t, f, "DROP INDEX "+index)
	}
	admissionSQL(t, f, `UPDATE metadata SET value='2' WHERE key='schema:review_execution_indexes'`)
}

func assertRetryIndexesRestored(t *testing.T, f *attemptFixture) {
	t.Helper()
	for _, index := range []string{"review_waits_live_retry", "review_transitions_exhaustion_time", "review_transitions_exhaustion_attempt", "review_operations_draft_probe"} {
		if got := attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index); got != 1 {
			t.Fatalf("retry index %s was not restored", index)
		}
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution_indexes'`).Scan(&version); err != nil || version != "4" {
		t.Fatal("retry index migration missing", version, err)
	}
}
