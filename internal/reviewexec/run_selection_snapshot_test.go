package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type selectionSnapshotReader struct {
	merged bool
	calls  *atomic.Int64
}

func (r selectionSnapshotReader) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	if r.calls != nil {
		r.calls.Add(1)
	}
	return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
}
func (r selectionSnapshotReader) Pull(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
	if r.calls != nil {
		r.calls.Add(1)
	}
	state := "open"
	if r.merged {
		state = "closed"
	}
	return reviewprovider.Pull{Number: n, RepositoryID: 801, State: state, Merged: r.merged, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
}
func selectionSnapshotFixture(t *testing.T) (*attemptFixture, core.Scope) {
	t.Helper()
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'selection-snapshot-worker','Builder')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	s := f.scope(t, attemptProject, f.writer, false)
	s.WorkerID = "selection-snapshot-worker"
	return f, s
}
func selectionSnapshotEnqueue(t *testing.T, f *attemptFixture, s core.Scope, key string) Receipt {
	t.Helper()
	b, err := f.e.EnqueueRun(t.Context(), s, key, RunRequest{Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if json.Unmarshal(b, &r) != nil {
		t.Fatal("invalid selection receipt")
	}
	return r
}
func TestSelectionSnapshotRealLinkedChildAndHistoricalAuthority(t *testing.T) {
	f, s := selectionSnapshotFixture(t)
	receipt := selectionSnapshotEnqueue(t, f, s, "snapshot-linked")
	if worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": selectionSnapshotReader{}}); err != nil || !worked {
		t.Fatal("actual source selection failed", err)
	}
	r, err := readRun(t.Context(), f.store.DB(), attemptProject, receipt.ID)
	if err != nil || r.State != "selected" || r.OperationID == "" {
		t.Fatal("selection did not link child", err)
	}
	before := restoredSelectionRows(t, f)
	if err = ValidateSelectionSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("genuine graph refused", err)
	}
	if !reflect.DeepEqual(before, restoredSelectionRows(t, f)) {
		t.Fatal("snapshot validation wrote data")
	}
	// Later policy/configuration changes and credential revocation are expected
	// history. Validation uses captured revisions, not present dispatch authority.
	f.configure(t, attemptProject, "test-capability", 1, true)
	if _, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE principal_id=?`, s.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if err = ValidateSelectionSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("historical authority incorrectly required live credentials", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = ValidateSelectionSnapshot(ctx, f.store.DB()); err == nil {
		t.Fatal("canceled validation succeeded")
	}
}
func restoredSelectionRows(t *testing.T, f *attemptFixture) [][]any {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT * FROM review_selections ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		v := make([]any, 15)
		ptr := make([]any, 15)
		for i := range v {
			ptr[i] = &v[i]
		}
		if err = rows.Scan(ptr...); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestSelectionSnapshotPagedRetainedHistoryAndTypedKeys(t *testing.T) {
	f, s := selectionSnapshotFixture(t)
	for i := range 66 {
		selectionSnapshotEnqueue(t, f, s, fmt.Sprintf("snapshot-empty-%d", i))
		if _, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": selectionSnapshotReader{merged: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateSelectionSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("retained second page refused", err)
	}
	// A BLOB key scans into Go text but cannot safely drive a string keyset.
	if _, err := f.store.DB().Exec(`UPDATE review_selections SET id=CAST(id AS BLOB) WHERE id=(SELECT id FROM review_selections ORDER BY id DESC LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSelectionSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("BLOB key was coerced/skipped")
	}
}
func TestSelectionSnapshotBindingAndShapeCorruptionRefused(t *testing.T) {
	for _, query := range []string{
		`UPDATE review_selections SET id=''`,
		`UPDATE review_selections SET worker_id='unretained-worker'`,
		`UPDATE review_selections SET actor_id='unretained-principal'`,
		`UPDATE review_selections SET capability_fingerprint=replace(capability_fingerprint,'a','f')||'x'`,
		`UPDATE review_selections SET expected_pull_number=4`,
		`UPDATE review_selections SET channel='hosted'`,
		`UPDATE review_operations SET actor_id='different-actor' WHERE id=(SELECT operation_id FROM review_selections)`,
		`UPDATE review_operations SET capability_fingerprint=CAST(capability_fingerprint AS BLOB) WHERE id=(SELECT operation_id FROM review_selections)`,
		`UPDATE review_operations SET head_sha='missing' WHERE id=(SELECT operation_id FROM review_selections)`,
	} {
		t.Run(fmt.Sprintf("case-%d", len(query)), func(t *testing.T) {
			f, s := selectionSnapshotFixture(t)
			selectionSnapshotEnqueue(t, f, s, "snapshot-corrupt")
			if _, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": selectionSnapshotReader{}}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := ValidateSelectionSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("corrupt selection graph accepted")
			}
		})
	}
}

// Execute one complete synchronous worker decision with still-valid source
// credentials/configuration: refusal cannot merely be caused by cancellation
// or revoked predecessor authority in the restored-instance fixture.
func TestSelectionSnapshotAbandonedRunNeverReentersSelection(t *testing.T) {
	f, s := selectionSnapshotFixture(t)
	receipt := selectionSnapshotEnqueue(t, f, s, "snapshot-abandon")
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		n, err := AbandonReviewRuns(ctx, tx, time.Now().UTC())
		if err == nil && n != 1 {
			return fmt.Errorf("pending selection not abandoned")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := restoredSelectionRows(t, f)
	var calls atomic.Int64
	worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": selectionSnapshotReader{calls: &calls}})
	if err != nil || worked || calls.Load() != 0 {
		t.Fatal("abandoned selection regained source authority", err)
	}
	var operations int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatal("abandoned selection created native child", err)
	}
	r, err := readRun(t.Context(), f.store.DB(), attemptProject, receipt.ID)
	if err != nil || r.State != "held" || r.Reason != "restore_abandoned" || r.OperationID != "" || !reflect.DeepEqual(before, restoredSelectionRows(t, f)) {
		t.Fatal("abandoned selection changed", err)
	}
}
