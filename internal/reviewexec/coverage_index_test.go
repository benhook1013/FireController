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
	// Reconstruct the actual version-four index in this owned fixture. Its
	// removal must preserve execution history and the original keyed receipt.
	admissionSQL(t, f, `CREATE INDEX review_operations_coverage ON review_operations(project_id,pull_number,channel,head_sha,state,id)`)
	admissionSQL(t, f, `UPDATE metadata SET value='4' WHERE key='schema:review_execution'`)
	admissionSQL(t, f, `DELETE FROM metadata WHERE key='schema:review_execution_indexes'`)
	journal := attemptState(t, f, a)
	var original []byte
	if err := f.store.DB().QueryRow(`SELECT response_json FROM receipts WHERE operation='reviews.request' LIMIT 1`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	cliAllowanceReopen(t, f)
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != "4" {
		t.Fatal("index migration renumbered execution-table migrations", version, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='review_operations_coverage'`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='review_operations_cli_admission'`) != 1 || attemptState(t, f, a) != journal {
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
