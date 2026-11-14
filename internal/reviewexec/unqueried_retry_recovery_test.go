package reviewexec

import (
	"reflect"
	"testing"
)

func TestInterruptedUnqueriedHostedRetryPreservesIntent(t *testing.T) {
	f, original, in, now := queuedCooldown(t)
	scope := f.scope(t, attemptProject, f.writer, false)
	before := bindingOperation(t, f, scope, original.OperationID)
	*now = in.Observation.RetryNotBefore
	retry := f.start(t, attemptProject, Receipt{ID: original.OperationID})
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := bindingOperation(t, f, scope, original.OperationID)
	if after.State != "queued" || after.AttemptID != "" || !reflect.DeepEqual(after.Wait, before.Wait) {
		t.Fatalf("lost original retry: %+v", after)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE id=? AND state='cancelled'`, retry.ID) != 1 {
		t.Fatal("interrupted attempt not terminal")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, retry.ID) != 0 {
		t.Fatal("reservation not released")
	}
	baseline, err := f.e.RetryBaseline(t.Context(), attemptProject, after.ID)
	if err != nil || baseline == nil || *baseline != in.Anchor {
		t.Fatal("lost baseline", baseline, err)
	}
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	stable := bindingOperation(t, f, scope, after.ID)
	if !reflect.DeepEqual(after, stable) {
		t.Fatal("repeat recovery mutated intent")
	}
	next := f.start(t, attemptProject, Receipt{ID: after.ID})
	if next.ID == retry.ID || next.OperationID != original.OperationID {
		t.Fatal("replacement identity", next)
	}
}

func TestInterruptedHostedRetryWithQueryDoesNotRequeue(t *testing.T) {
	f, original, in, now := queuedCooldown(t)
	*now = in.Observation.RetryNotBefore
	retry := f.start(t, attemptProject, Receipt{ID: original.OperationID})
	// A raw retained query row is enough to fence this recovery even if its
	// payload cannot be decoded. Never infer absence from malformed evidence.
	admissionSQL(t, f, `INSERT INTO review_hosted_allowance_queries SELECT 'review_query_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',project_id,?,operation_id,capability_fingerprint,revision,'{}' FROM review_hosted_allowance_queries WHERE attempt_id=?`, retry.ID, original.ID)
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), original.OperationID)
	if after.State == "queued" {
		t.Fatal("query-bearing attempt requeued")
	}
}
