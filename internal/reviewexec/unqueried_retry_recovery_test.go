package reviewexec

import (
	"reflect"
	"testing"
	"time"
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
	assertCLIWaitSnapshot(t, f)
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

func TestCancelledUnqueriedRetryHistoryRequiresExactProof(t *testing.T) {
	for _, damage := range []string{"reason", "first-reason", "missing-first", "extra-journal", "before-baseline", "external", "intent", "revision", "account", "query", "observation", "preflight", "cli-preflight", "evidence", "reservation"} {
		t.Run(damage, func(t *testing.T) {
			f, original, in, now := queuedCooldown(t)
			*now = in.Observation.RetryNotBefore
			retry := f.start(t, attemptProject, Receipt{ID: original.OperationID})
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			o := cliWaitRead(t, f, original.OperationID)
			switch damage {
			case "reason":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='user_cancelled' WHERE attempt_id=? AND revision=2`, retry.ID)
			case "first-reason":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='not_preparing' WHERE attempt_id=? AND revision=1`, retry.ID)
			case "missing-first":
				admissionSQL(t, f, `DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, retry.ID)
			case "extra-journal":
				admissionSQL(t, f, `INSERT INTO review_execution_transitions SELECT attempt_id,3,state,reason,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, retry.ID)
			case "before-baseline":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=1`, now.Add(-24*time.Hour).Format(time.RFC3339Nano), retry.ID)
			case "external":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET external_id='external-trigger' WHERE id=?`, retry.ID)
			case "intent":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET intent='issued-intent' WHERE id=?`, retry.ID)
			case "revision":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET revision=3 WHERE id=?`, retry.ID)
			case "account":
				admissionSQL(t, f, `UPDATE review_execution_attempts SET account='other-account' WHERE id=?`, retry.ID)
			case "query":
				admissionSQL(t, f, `INSERT INTO review_hosted_allowance_queries SELECT 'review_query_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',project_id,?,operation_id,capability_fingerprint,revision,'{}' FROM review_hosted_allowance_queries WHERE attempt_id=?`, retry.ID, original.ID)
			case "observation":
				admissionSQL(t, f, `INSERT INTO review_hosted_observations VALUES('bad-observation',?,?,?,1,'bad',1,'{}')`, attemptProject, retry.ID, o.ID)
			case "preflight":
				admissionSQL(t, f, `INSERT INTO review_hosted_preflight VALUES(?,?,'{}')`, retry.ID, attemptProject)
			case "cli-preflight":
				admissionSQL(t, f, `INSERT INTO review_cli_preflight VALUES(?,?,'{}')`, retry.ID, attemptProject)
			case "evidence":
				admissionSQL(t, f, `INSERT INTO review_execution_evidence VALUES('bad-evidence',?,?,?,'{}','bad')`, attemptProject, retry.ID, o.ID)
			case "reservation":
				admissionSQL(t, f, `INSERT INTO review_execution_reservations VALUES(?,901,2,'hosted',?,?)`, retry.Provider, retry.Account, retry.ID)
			}
			before := deadlineRuntimeState(t, f)
			if wait, err := readOperationWait(t.Context(), f.store.DB(), o); wait != nil || err == nil {
				t.Fatal("runtime accepted damaged cancelled history", wait, err)
			}
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateOperationWaitsSnapshot(t.Context(), tx)
			tx.Rollback()
			if err == nil {
				t.Fatal("snapshot accepted damaged cancelled history")
			}
			if before != deadlineRuntimeState(t, f) {
				t.Fatal("validation mutated retained state")
			}
		})
	}
}
