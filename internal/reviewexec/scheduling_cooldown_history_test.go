package reviewexec

import (
	"reflect"
	"testing"
)

func TestSchedulingHoldKeepsCooldownHistoryStrict(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed", "automatic_scheduling_unavailable"} {
			for _, damage := range []string{"none", "first-reason", "missing-first", "extra-transition", "intent", "reservation", "evidence"} {
				t.Run(channel+"/"+reason+"/"+damage, func(t *testing.T) {
					f, original, now := deadlineRuntimeFixture(t, channel)
					before := cliWaitRead(t, f, original.OperationID)
					*now = cliWaitDue(t, before)
					retry := f.start(t, attemptProject, Receipt{ID: original.OperationID})
					if _, err := f.e.Advance(t.Context(), attemptProject, retry.ID, retry.Revision, "held", reason, ""); err != nil {
						t.Fatal(err)
					}
					after := cliWaitRead(t, f, original.OperationID)
					if after.State != "queued" || after.AttemptID != "" || !reflect.DeepEqual(before.Wait, after.Wait) {
						t.Fatal("lost original timer", after)
					}
					switch damage {
					case "first-reason":
						admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='not-preparing' WHERE attempt_id=? AND revision=1`, retry.ID)
					case "missing-first":
						admissionSQL(t, f, `DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, retry.ID)
					case "extra-transition":
						admissionSQL(t, f, `INSERT INTO review_execution_transitions SELECT attempt_id,3,state,reason,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, retry.ID)
					case "intent":
						admissionSQL(t, f, `UPDATE review_execution_attempts SET intent='issued' WHERE id=?`, retry.ID)
					case "reservation":
						admissionSQL(t, f, `INSERT INTO review_execution_reservations VALUES(?,901,2,?,?,?)`, retry.Provider, channel, retry.Account, retry.ID)
					case "evidence":
						admissionSQL(t, f, `INSERT INTO review_execution_evidence VALUES('bad-evidence',?,?,?,'{}','bad')`, attemptProject, retry.ID, after.ID)
					}
					state := deadlineRuntimeState(t, f)
					wait, err := readOperationWait(t.Context(), f.store.DB(), after)
					if damage == "none" {
						if err != nil || !reflect.DeepEqual(wait, before.Wait) {
							t.Fatal("valid history refused", wait, err)
						}
					} else if err == nil || wait != nil {
						t.Fatal("runtime accepted corruption", wait, err)
					}
					tx, err := f.store.DB().BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					err = ValidateOperationWaitsSnapshot(t.Context(), tx)
					if state != deadlineRuntimeStateThrough(t, tx) {
						t.Fatal("validation wrote inside the retained-history transaction")
					}
					if rollbackErr := tx.Rollback(); rollbackErr != nil {
						t.Fatal(rollbackErr)
					}
					if (err == nil) != (damage == "none") {
						t.Fatal("snapshot history classification", err)
					}
					if state != deadlineRuntimeState(t, f) {
						t.Fatal("validation mutated retained history")
					}
				})
			}
		}
	}
}
