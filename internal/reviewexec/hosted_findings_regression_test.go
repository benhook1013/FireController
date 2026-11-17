package reviewexec

import (
	"context"
	"database/sql"
	"testing"
)

type comparisonSnapshotQuery struct {
	db    *sql.DB
	calls int
}

func (q *comparisonSnapshotQuery) QueryRowContext(ctx context.Context, sql string, args ...any) *sql.Row {
	q.calls++
	return q.db.QueryRowContext(ctx, sql, args...)
}

func TestRetainedComparisonSchemaUsesOneSnapshotStatement(t *testing.T) {
	f := newAttemptFixture(t)
	q := &comparisonSnapshotQuery{db: f.store.DB()}
	installed, err := retainedComparisonSchema(t.Context(), q)
	if err != nil || !installed || q.calls != 1 {
		t.Fatalf("schema facts not one snapshot: %v %v %d", installed, err, q.calls)
	}
	admissionSQL(t, f, `DELETE FROM metadata WHERE key='schema:review_comparison'`)
	q.calls = 0
	_, err = retainedComparisonSchema(t.Context(), q)
	attemptCode(t, err, "schema_invalid")
	if q.calls != 1 {
		t.Fatalf("damaged schema used %d snapshots", q.calls)
	}
}

func TestMalformedCLICooldownDonorIsAccountLocal(t *testing.T) {
	f, a, _, _ := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	timerControl(t, f, a.OperationID, "cancel", "cancel-malformed-donor")
	same := f.enqueue(t, attemptProject, "same-account-candidate", "cli", 4, 1)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	healthy := f.enqueue(t, otherAttemptProject, "healthy-account", "cli", 2, 1)
	// Simulate retained corruption without relaxing ordinary runtime writes.
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"PRAGMA ignore_check_constraints=ON", "UPDATE review_operations SET allow_out_of_order=2 WHERE id='" + a.OperationID + "'", "PRAGMA ignore_check_constraints=OFF"} {
		if _, err := conn.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	operations, err := f.e.pending(t.Context())
	if err != nil {
		t.Fatal("damaged donor stopped independent scan", err)
	}
	found := false
	for _, o := range operations {
		if o.ID == same.ID {
			t.Fatal("damaged account admitted")
		}
		if o.ID == healthy.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("healthy account hidden")
	}
}
