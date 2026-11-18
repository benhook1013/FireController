package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestControlsRefuseLostCooldownBaseline(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, action := range []string{"pause", "resume", "cancel"} {
			t.Run(channel+"/"+action, func(t *testing.T) {
				f, a, _ := deadlineRuntimeFixture(t, channel)
				o := cliWaitRead(t, f, a.OperationID)
				admissionSQL(t, f, `DELETE FROM review_operation_waits WHERE operation_id=?`, o.ID)
				before := deadlineRuntimeState(t, f)
				_, err := f.e.ControlOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), "lost-baseline", o.ID, OperationControl{ExpectedRevision: o.Revision, Action: action})
				if !isWaitUnavailable(err) {
					t.Fatal("missing baseline accepted", err)
				}
				if before != deadlineRuntimeState(t, f) {
					t.Fatal("refused control mutated history")
				}
			})
		}
	}
}

func TestFabricatedDraftReasonIsNotHistoricalProof(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "fake-draft", "cli", 2, 1)
	admissionSQL(t, f, `UPDATE review_operations SET reason='cli_draft_wait' WHERE id=?`, receipt.ID)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	o, err := readOperationBase(t.Context(), tx, attemptProject, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOperationWaitHistory(t.Context(), tx, o); !isWaitUnavailable(err) {
		t.Fatal("reason without proof accepted", err)
	}
}

func TestWorkspaceRetentionReportsMissingCurrentTransition(t *testing.T) {
	f, a, _ := evidenceFixture(t, "cli", true)
	admissionSQL(t, f, `UPDATE review_execution_attempts SET state='completed' WHERE id=?`, a.ID)
	admissionSQL(t, f, `UPDATE review_operations SET state='completed' WHERE attempt_id=?`, a.ID)
	admissionSQL(t, f, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID)
	admissionSQL(t, f, `DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, a.ID)
	allowed, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, time.Now().Add(time.Hour))
	if allowed || err == nil {
		t.Fatal("missing journal silently treated as ineligible", allowed, err)
	}
}

func TestCLICooldownClockRollbackKeepsBackoffProgression(t *testing.T) {
	f, first, now := deadlineRuntimeFixture(t, "cli")
	before := cliWaitRead(t, f, first.OperationID)
	due := cliWaitDue(t, before)
	*now = due
	next := f.start(t, attemptProject, Receipt{ID: first.OperationID})
	input := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 7, FullCapacityInMS: 3600000, RollingWindowMS: 86400000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, next.ID, next.Revision, input); err != nil {
		t.Fatal(err)
	}
	*now = due.Add(-time.Hour)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, next.ID, next.Revision); err != nil {
		t.Fatal(err)
	}
	after := cliWaitRead(t, f, first.OperationID)
	if got := cliWaitDue(t, after); !got.Equal(due.Add(2 * time.Minute)) {
		t.Fatal("rollback reset backoff", due, got)
	}
}

func TestFinalAdmissionDamagedConfigurationStaysCandidateLocal(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "admission-history", "cli", 2, 1)
	driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		admissionSQL(t, f, `UPDATE review_execution_config SET body_json='broken-retained-history' WHERE project_id=?`, attemptProject)
		return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
		t.Fatal("candidate corruption stopped supervisor", err)
	}
	if driver.calls.Load() != 0 || supervisorOperationState(t, f, receipt.ID) != "preparing" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("damaged admission dispatched or destroyed retained preparation")
	}
}

func TestRestoreChecksPriorCooldownHistory(t *testing.T) {
	for _, damage := range []string{"valid", "allowance", "journal"} {
		t.Run(fmt.Sprint(damage), func(t *testing.T) {
			f, a, now := deadlineRuntimeFixture(t, "cli")
			timerControl(t, f, a.OperationID, "pause", "restore-pause")
			timerControl(t, f, a.OperationID, "resume", "restore-resume")
			*now = cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
			next := f.start(t, attemptProject, Receipt{ID: a.OperationID})
			if damage == "journal" {
				admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='not-preparing' WHERE attempt_id=? AND revision=1`, a.ID)
			} else if damage == "allowance" {
				admissionSQL(t, f, `UPDATE review_cli_preflight SET body_json='{}' WHERE attempt_id=?`, a.ID)
			}
			restoredHeld(t, f)
			before := deadlineRuntimeState(t, f)
			_, err := restoredCall(t, f, now.Add(time.Minute))
			if damage != "valid" {
				if err == nil || before != deadlineRuntimeState(t, f) {
					t.Fatal("damaged prior history reconciled", err)
				}
			} else if err != nil {
				t.Fatal("legitimate prior cooldown and controls refused", next, err)
			}
		})
	}
}

func TestPendingScanRollbackClassification(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(pendingScanError(ctx, sql.ErrTxDone), context.DeadlineExceeded) {
		t.Fatal("local rollback became storage failure")
	}
	storage := errors.New("actual disk failure")
	if pendingScanError(ctx, storage) != storage {
		t.Fatal("deadline hid actual storage failure")
	}
	if pendingScanError(t.Context(), sql.ErrTxDone) != sql.ErrTxDone {
		t.Fatal("unexplained rollback hidden")
	}
}
