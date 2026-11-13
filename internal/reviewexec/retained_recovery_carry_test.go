package reviewexec

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAttemptRecoveryCancelsCorruptOptionalAllowance(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, corruption := range []string{"malformed", "binding", "oversized", "blob"} {
			t.Run(channel+"/"+corruption, func(t *testing.T) {
				f, a := interruptedAllowanceFixture(t, channel)
				table := "review_" + channel + "_preflight"
				switch corruption {
				case "malformed":
					admissionSQL(t, f, `UPDATE `+table+` SET body_json='{' WHERE attempt_id=?`, a.ID)
				case "binding":
					admissionSQL(t, f, `UPDATE `+table+` SET body_json=replace(body_json,'exec-project-one','exec-project-two') WHERE attempt_id=?`, a.ID)
				case "oversized":
					limit := 8192
					if channel == "hosted" {
						limit = 16384
					}
					admissionSQL(t, f, `UPDATE `+table+` SET body_json=? WHERE attempt_id=?`, strings.Repeat("x", limit+1), a.ID)
				case "blob":
					admissionSQL(t, f, `UPDATE `+table+` SET body_json=CAST(body_json AS BLOB) WHERE attempt_id=?`, a.ID)
				}
				o := cliWaitRead(t, f, a.OperationID)
				var err error
				if channel == "cli" {
					_, err = readCLIAllowanceRecord(t.Context(), f.store.DB(), o, a)
				} else {
					_, err = readHostedAllowanceRecord(t.Context(), f.store.DB(), o, a)
				}
				if !interruptedAllowanceUnavailable(err) || (corruption == "oversized" || corruption == "blob") && !storedAllowanceUnavailable(err) {
					t.Fatal("unusable retained body lost recovery classification", err)
				}
				assertCorruptAllowanceRecovery(t, f, a)
			})
		}
	}
}

func assertCorruptAllowanceRecovery(t *testing.T, f *attemptFixture, a Attempt) {
	t.Helper()
	retained := func() string {
		var state string
		for _, table := range []string{"review_cli_preflight", "review_hosted_preflight", "review_hosted_allowance_queries", "review_hosted_observations", "review_execution_evidence", "review_round_controls", "review_round_changes", "review_round_operation_bindings", "review_round_completions"} {
			state += snapshotTable(t, f, table)
		}
		return state
	}
	before := retained()
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal("corrupt optional evidence blocked recovery", err)
	}
	o := cliWaitRead(t, f, a.OperationID)
	recovered, err := readAttempt(t.Context(), f.store.DB(), attemptProject, a.ID)
	if err != nil || o.State != "cancelled" || o.Reason != "supervisor_interrupted" || o.Wait != nil || o.AttemptID != a.ID || recovered.State != "cancelled" || recovered.Revision != a.Revision+1 || recovered.Anchor != nil || recovered.Intent != "" || recovered.ExternalID != "" {
		t.Fatal("corrupt optional evidence did not safely cancel unused preparation", o, recovered, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operation_waits`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
		t.Fatal("recovery created an attempt, retry, reservation, evidence or review credit")
	}
	if before != retained() {
		t.Fatal("recovery rewrote evidence or review progress")
	}
	settledState := func() string {
		return retained() + snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_transitions") + snapshotTable(t, f, "review_execution_reservations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "events") + snapshotTable(t, f, "receipts")
	}
	settled := settledState()
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal("repeated recovery", err)
	}
	intent := ""
	if o.Channel == "hosted" {
		intent = "synthetic-intent-" + a.ID
	}
	_, err = f.e.Admit(t.Context(), attemptProject, a.ID, recovered.Revision, attemptAnchor(attemptProject, o.PullNumber), intent)
	attemptCode(t, err, "review_conflict")
	driver := &controlledDriver{}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
		t.Fatal("post-recovery supervisor pass", err)
	}
	if driver.prepares.Load() != 0 || driver.calls.Load() != 0 || settled != settledState() {
		t.Fatal("repeated recovery or supervisor pass changed settled work or contacted provider")
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
				f, a := interruptedAllowanceFixture(t, channel)
				o := cliWaitRead(t, f, a.OperationID)
				before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "review_execution_reservations")
				ctx := t.Context()
				if failure == "storage" {
					admissionSQL(t, f, `ALTER TABLE review_`+channel+`_preflight RENAME TO unavailable_preflight`)
				} else {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				var readErr error
				if channel == "cli" {
					_, readErr = readCLIAllowanceRecord(ctx, f.store.DB(), o, a)
				} else {
					_, readErr = readHostedAllowanceRecord(ctx, f.store.DB(), o, a)
				}
				if readErr == nil || interruptedAllowanceUnavailable(readErr) || failure == "context" && !errors.Is(readErr, context.Canceled) {
					t.Fatal("allowance reader suppressed or reclassified query failure", readErr)
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
	for _, corruption := range []string{"malformed", "binding", "oversized", "blob"} {
		t.Run(corruption, func(t *testing.T) {
			f, a := interruptedAllowanceFixture(t, "hosted")
			o := cliWaitRead(t, f, a.OperationID)
			record, err := readHostedAllowanceRecord(t.Context(), f.store.DB(), o, a)
			if err != nil {
				t.Fatal(err)
			}
			query, err := readHostedQuery(t.Context(), f.store.DB(), a, o)
			if err != nil {
				t.Fatal(err)
			}
			switch corruption {
			case "malformed":
				admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json='{' WHERE attempt_id=?`, a.ID)
			case "binding":
				admissionSQL(t, f, `UPDATE review_hosted_observations SET capture_bytes=capture_bytes+1 WHERE attempt_id=?`, a.ID)
			case "oversized":
				admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=? WHERE attempt_id=?`, strings.Repeat("x", 24577), a.ID)
			case "blob":
				admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=CAST(body_json AS BLOB) WHERE attempt_id=?`, a.ID)
			}
			if err := validateCooldownReference(t.Context(), f.store.DB(), a, o, query, record.Input); !storedAllowanceUnavailable(err) {
				t.Fatal("unusable reference body lost allowance classification", err)
			}
			assertCorruptAllowanceRecovery(t, f, a)
		})
	}
}

func TestRetainedCoreMissingRecoveryDriverIsolatedAtStartup(t *testing.T) {
	f, a, _ := cooldownObservationFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-unused", "cli", 2, 1))
	before := attemptState(t, f, a)
	delete(f.e.capabilities, "test-capability")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.e.Start(ctx, map[string]Driver{"other-account": &controlledDriver{}})
	if err != nil {
		t.Fatal("missing original driver blocked unrelated recovery", err)
	}
	cancel()
	<-done
	if before != attemptState(t, f, a) {
		t.Fatal("excluded preparation/reservation changed")
	}
	got, err := readAttempt(t.Context(), f.store.DB(), otherAttemptProject, other.ID)
	if err != nil || got.State != "cancelled" {
		t.Fatal("unrelated unused preparation not recovered", got, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id='`+a.ID+`'`) != 1 {
		t.Fatal("lost original reservation")
	}
}

type retainedCoreFailedRecovery struct {
	controlledDriver
	failure error
}

func (d *retainedCoreFailedRecovery) RecoverPreparation(context.Context, Operation, Attempt) error {
	return d.failure
}
func TestRetainedCoreReplayFailureStillBlocksStartup(t *testing.T) {
	for _, failure := range []error{errors.New("retention unavailable"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			f, a, _ := cooldownObservationFixture(t)
			before := attemptState(t, f, a)
			drivers := map[string]Driver{}
			for key := range f.e.capabilities {
				drivers[key] = &retainedCoreFailedRecovery{failure: failure}
			}
			done, err := f.e.Start(t.Context(), drivers)
			if done != nil || !errors.Is(err, failure) || f.e.running.Load() {
				t.Fatal("replay failure did not block startup", err)
			}
			if before != attemptState(t, f, a) {
				t.Fatal("failed replay changed original attempt")
			}
		})
	}
}
