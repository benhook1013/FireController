package reviewexec

import (
	"context"
	"reflect"
	"testing"
)

func TestSchedulingAdmissionRechecksControlsAfterPreparation(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, change := range []string{"explicit-pause", "automatic-pause", "disable", "disable-reenable", "revoke", "lost-management", "malformed-state"} {
			t.Run(channel+"/"+change, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				automatic := change != "explicit-pause"
				var id string
				if automatic {
					if channel == "hosted" {
						schedulingEnable(t, f, 0, true)
						schedulingGenerate(t, f, true)
					} else {
						schedulingCLIEnable(t, f, 0, true)
						schedulingCLIGenerate(t, f, true)
					}
					selectionNext(t, f, &refreshTestReader{})
					id = schedulingLatest(t, f).OperationID
				} else {
					id = f.enqueue(t, attemptProject, "late-explicit-pause", channel, 2, 1).ID
				}
				before := roundOperation(t, f, id)
				attribution := snapshotTable(t, f, "review_automatic_selections")
				waits := snapshotTable(t, f, "review_operation_waits")
				reason := "automation_authorization_changed"
				if change == "explicit-pause" || change == "automatic-pause" {
					reason = "pull_scheduling_paused"
				}
				if change == "malformed-state" {
					reason = "automatic_scheduling_unavailable"
				}
				var preparedAttempt Attempt
				driver := &controlledDriver{prepare: func(ctx context.Context, op Operation, _ Configuration, a Attempt) (Preparation, error) {
					if _, err := f.e.ExecutionContext(ctx, op.ProjectID, op.ID); err != nil {
						t.Fatal("preparation did not begin with valid controls", err)
					}
					preparedAttempt = a
					switch change {
					case "explicit-pause", "automatic-pause":
						schedulingPause(t, f, 2, 0, true)
					case "disable", "disable-reenable":
						if channel == "hosted" {
							schedulingEnable(t, f, 1, false)
							if change == "disable-reenable" {
								schedulingEnable(t, f, 2, true)
							}
						} else {
							schedulingCLIEnable(t, f, 1, false)
							if change == "disable-reenable" {
								schedulingCLIEnable(t, f, 2, true)
							}
						}
					case "revoke":
						admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
					case "lost-management":
						admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
					case "malformed-state":
						admissionSQL(t, f, `UPDATE review_execution_config SET body_json='{' WHERE project_id=?`, attemptProject)
					}
					intent := ""
					if channel == "hosted" {
						intent = "unposted-preparation-intent"
					}
					return Preparation{Anchor: attemptAnchor(op.ProjectID, op.PullNumber), Intent: intent}, nil
				}}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
					t.Fatal("late control refusal became a worker failure", err)
				}
				after := roundOperation(t, f, id)
				if after.State != "queued" || after.Reason != reason || after.AttemptID != "" || after.ActorID != before.ActorID || after.WorkerID != before.WorkerID || after.HeadSHA != before.HeadSHA || after.ConfigurationRevision != before.ConfigurationRevision || after.Channel != before.Channel || driver.prepares.Load() != 1 || driver.calls.Load() != 0 {
					t.Fatal("late refusal lost the request or dispatched", before, after)
				}
				if attribution != snapshotTable(t, f, "review_automatic_selections") || waits != snapshotTable(t, f, "review_operation_waits") || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
					t.Fatal("late refusal changed attribution/waits, duplicated work or stranded capacity")
				}
				var state, anchor, intent, external string
				if err := f.store.DB().QueryRow(`SELECT state,anchor_json,intent,external_id FROM review_execution_attempts WHERE id=?`, preparedAttempt.ID).Scan(&state, &anchor, &intent, &external); err != nil || state != "held" || anchor != "null" || intent != "" || external != "" {
					t.Fatal("unused attempt recorded dispatch intent", state, anchor, intent, external, err)
				}
				if change == "explicit-pause" || change == "automatic-pause" {
					schedulingPause(t, f, 2, 1, false)
					driver.prepare = nil
					driver.dispatch = func(ctx context.Context, op Operation, _ Attempt, record func(context.Context, string) error) (Result, error) {
						external := ""
						if op.Channel == "hosted" {
							external = "synthetic_resume_trigger"
							if err := record(ctx, external); err != nil {
								return Result{}, err
							}
						}
						return Result{State: "completed", Reason: "synthetic_capture", ExternalID: external}, nil
					}
					if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil || driver.calls.Load() != 1 || roundOperation(t, f, id).State != "completed" {
						t.Fatal("resume did not finish the same queued operation", err)
					}
				} else if change == "disable-reenable" {
					attempt, err := f.e.StartAttempt(t.Context(), attemptProject, id)
					if err != nil || attempt.ID != "" {
						t.Fatal("re-enabling resurrected interrupted authorization", attempt, err)
					}
				}
			})
		}
	}
}

func TestSchedulingAdmitRefusalLeavesPreparationUntouched(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	op := f.enqueue(t, attemptProject, "direct-late-pause", "cli", 2, 1)
	a := f.start(t, attemptProject, op)
	schedulingPause(t, f, 2, 0, true)
	before := roundTables(t, f)
	_, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	attemptCode(t, err, "pull_scheduling_paused")
	if !reflect.DeepEqual(before, roundTables(t, f)) {
		t.Fatal("refused admission changed preparing state before supervisor settlement")
	}
	if err := f.e.finishPreparation(a, "held", "pull_scheduling_paused"); err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("refused preparation retained capacity")
	}
}

func TestSchedulingAdmissionDatabaseFailureStillStopsWithReservation(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	op := f.enqueue(t, attemptProject, "admission-storage-failure", "cli", 2, 1)
	driver := &controlledDriver{prepare: func(ctx context.Context, op Operation, _ Configuration, _ Attempt) (Preparation, error) {
		admissionSQL(t, f, `ALTER TABLE review_execution_config RENAME TO unavailable_execution_config`)
		return Preparation{Anchor: attemptAnchor(op.ProjectID, op.PullNumber)}, nil
	}}
	err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver})
	attemptCode(t, err, "review_persistence_failed")
	after := roundOperation(t, f, op.ID)
	if after.State != "preparing" || after.AttemptID == "" || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("database failure was suppressed as a project-local refusal", after)
	}
}
