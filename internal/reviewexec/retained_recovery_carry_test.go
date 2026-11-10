package reviewexec

import (
	"context"
	"errors"
	"testing"
)

func TestAttemptRecoveryCancelsCorruptOptionalAllowance(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, corruption := range []string{"malformed", "binding"} {
			t.Run(channel+"/"+corruption, func(t *testing.T) {
				f, a := interruptedAllowanceFixture(t, channel)
				table := "review_" + channel + "_preflight"
				query := `UPDATE ` + table + ` SET body_json='{' WHERE attempt_id=?`
				if corruption == "binding" {
					query = `UPDATE ` + table + ` SET body_json=replace(body_json,'exec-project-one','exec-project-two') WHERE attempt_id=?`
				}
				admissionSQL(t, f, query, a.ID)
				before := snapshotTable(t, f, table)
				if err := f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
				o := cliWaitRead(t, f, a.OperationID)
				if o.State != "cancelled" || o.Reason != "supervisor_interrupted" || o.Wait != nil || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE id=? AND state='cancelled' AND revision=2`, a.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
					t.Fatal("corrupt optional evidence did not safely cancel preparation", o)
				}
				if before != snapshotTable(t, f, table) {
					t.Fatal("recovery rewrote evidence")
				}
				if err := f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func interruptedAllowanceFixture(t *testing.T, channel string) (*attemptFixture, Attempt) {
	t.Helper()
	if channel == "cli" {
		f, a, _, _ := cliWaitFixture(t)
		return f, a
	}
	f, a, in := cooldownObservationFixture(t)
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestAttemptRecoveryPreservesAllowanceStorageAndContextFailures(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, failure := range []string{"storage", "context"} {
			t.Run(channel+"/"+failure, func(t *testing.T) {
				f, _ := interruptedAllowanceFixture(t, channel)
				before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations")
				ctx := t.Context()
				if failure == "storage" {
					admissionSQL(t, f, `ALTER TABLE review_`+channel+`_preflight RENAME TO unavailable_preflight`)
				} else {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				err := f.e.RecoverInterrupted(ctx)
				if err == nil || interruptedAllowanceUnavailable(err) {
					t.Fatal("failure was suppressed", err)
				}
				if failure == "context" {
					attemptCode(t, err, "overloaded")
				}
				if after := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations"); after != before {
					t.Fatal("failed recovery changed work")
				}
			})
		}
	}
}

func TestAttemptRecoveryPreservesCooldownReferenceStorageFailure(t *testing.T) {
	f, a := interruptedAllowanceFixture(t, "hosted")
	admissionSQL(t, f, `ALTER TABLE review_hosted_observations RENAME TO unavailable_observations`)
	before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations")
	if err := f.e.RecoverInterrupted(t.Context()); err == nil || interruptedAllowanceUnavailable(err) {
		t.Fatal("reference storage failure suppressed", err)
	}
	if after := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations"); after != before {
		t.Fatal("failed recovery changed work", a.ID)
	}
}

func TestCooldownReferencePreservesContextFailureAndClassifiesMissingEvidence(t *testing.T) {
	f, a, in := cooldownObservationFixture(t)
	o := cliWaitRead(t, f, a.OperationID)
	query, err := readHostedQuery(t.Context(), f.store.DB(), a, o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := validateCooldownReference(ctx, f.store.DB(), a, o, query, in); !errors.Is(err, context.Canceled) {
		t.Fatal("reference lost cancellation", err)
	}
	admissionSQL(t, f, `DELETE FROM review_hosted_observations WHERE attempt_id=?`, a.ID)
	if err := validateCooldownReference(t.Context(), f.store.DB(), a, o, query, in); !storedAllowanceUnavailable(err) {
		t.Fatal("missing reference lost unavailable classification", err)
	}
}

func TestAttemptRecoveryCancelsCorruptCooldownReference(t *testing.T) {
	for _, corruption := range []string{"malformed", "binding"} {
		t.Run(corruption, func(t *testing.T) {
			f, a := interruptedAllowanceFixture(t, "hosted")
			query := `UPDATE review_hosted_observations SET body_json='{' WHERE attempt_id=?`
			if corruption == "binding" {
				query = `UPDATE review_hosted_observations SET capture_bytes=capture_bytes+1 WHERE attempt_id=?`
			}
			admissionSQL(t, f, query, a.ID)
			before := snapshotTable(t, f, "review_hosted_observations")
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal("corrupt optional reference blocked recovery", err)
			}
			o := cliWaitRead(t, f, a.OperationID)
			if o.State != "cancelled" || o.Reason != "supervisor_interrupted" || o.Wait != nil || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE id=? AND state='cancelled' AND revision=2`, a.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
				t.Fatal("corrupt reference did not safely cancel undispatched preparation", o)
			}
			if before != snapshotTable(t, f, "review_hosted_observations") {
				t.Fatal("recovery rewrote referenced evidence")
			}
			settled := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_transitions")
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			if settled != snapshotTable(t, f, "review_operations")+snapshotTable(t, f, "review_execution_attempts")+snapshotTable(t, f, "review_execution_transitions") {
				t.Fatal("recovery was not idempotent")
			}
		})
	}
}
