package reviewexec

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSchedulingPreparationGuardRetainsOperation(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, change := range []string{"pause", "disable", "withdraw"} {
			t.Run(channel+"/"+change, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				on := true
				revision := int64(0)
				scope := selectionScope(t, f, attemptProject, f.writer, "write")
				_, err := f.e.ConfigureScheduling(t.Context(), scope, "enable-both", SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &on, AutomaticCLI: &on})
				if err != nil {
					t.Fatal(err)
				}
				// Select the requested automatic channel through the normal scheduler.
				readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
				if channel == "cli" {
					_, err = f.e.generateAutomaticCLI(t.Context(), readers)
				} else {
					_, err = f.e.generateAutomaticHosted(t.Context(), readers)
				}
				if err != nil {
					t.Fatal(err)
				}
				selectionNext(t, f, &refreshTestReader{})
				r := schedulingLatest(t, f)
				if r.OperationID == "" || r.Channel != channel {
					t.Fatal(r)
				}
				deadline := ""
				admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,NULL,?,0)`, r.OperationID, deadline)
				before, err := f.e.ReadOperation(t.Context(), scope, r.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				expected := "automation_authorization_changed"
				if change == "pause" {
					expected = "pull_scheduling_paused"
				}
				var original Attempt
				driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
					original = a
					switch change {
					case "pause":
						schedulingPause(t, f, 2, 0, true)
					case "disable":
						off, rev := false, int64(1)
						_, err := f.e.ConfigureScheduling(ctx, scope, "disable-both", SchedulingRequest{ExpectedRevision: &rev, AutomaticHosted: &off, AutomaticCLI: &off})
						if err != nil {
							t.Fatal(err)
						}
					case "withdraw":
						admissionSQL(t, f, `UPDATE grants SET can_manage=0 WHERE credential_id='exec-writer-cred'`)
					}
					_, err := f.e.ExecutionContext(ctx, o.ProjectID, o.ID)
					attemptCode(t, err, expected)
					return Preparation{}, err
				}}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
					t.Fatal(err)
				}
				after, err := f.e.ReadOperation(t.Context(), scope, r.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != "queued" || after.Reason != expected || after.AttemptID != "" {
					t.Fatalf("guard lost retained request: %+v", after)
				}
				if after.ID != before.ID || after.ActorID != before.ActorID || after.WorkerID != before.WorkerID || after.HeadSHA != before.HeadSHA || after.ConfigurationRevision != before.ConfigurationRevision || !reflect.DeepEqual(after.Wait, before.Wait) {
					t.Fatal("changed original binding or wait", before, after)
				}
				if driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
					t.Fatal("dispatched or leaked reservation")
				}
				var state, reason string
				if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, original.ID).Scan(&state, &reason); err != nil || state != "held" || reason != expected {
					t.Fatal(state, reason, err)
				}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil || driver.prepares.Load() != 1 {
					t.Fatal("blocked operation prepared again", err)
				}
				if change == "pause" {
					schedulingPause(t, f, 2, 1, false)
					retry, err := f.e.StartAttempt(t.Context(), attemptProject, after.ID)
					if err != nil || retry.ID == "" || retry.ID == original.ID || retry.OperationID != before.ID {
						t.Fatal("same operation could not resume", retry, err)
					}
				}
			})
		}
	}
}

func TestSchedulingQueuedPauseDiagnosticPrecedence(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, wait := range []string{"scheduler", "queue", "draft"} {
			t.Run(channel+"/"+wait, func(t *testing.T) {
				f := schedulingFixture(t)
				pull := int64(2)
				if wait == "queue" {
					f.enqueue(t, attemptProject, "earlier", channel, 2, 1)
					pull = 4
				}
				r := f.enqueue(t, attemptProject, "queued-pause", channel, pull, 1)
				scope := selectionScope(t, f, attemptProject, f.writer, "write")
				deadline := ""

				admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,NULL,?,0)`, r.ID, deadline)
				if wait == "draft" {
					admissionSQL(t, f, `UPDATE review_operations SET reason=? WHERE id=?`, cliDraftWait, r.ID)
				}
				schedulingPause(t, f, pull, 0, true)
				check := func(want string) {
					t.Helper()
					if got := f.e.queuedDiagnostic(t.Context(), scope, r.ID, r.Revision); got != want {
						t.Fatalf("diagnostic=%q want=%q", got, want)
					}
				}
				check("pull_scheduling_paused")
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.paused',json('true')) WHERE project_id=?`, attemptProject)
				check("project_paused")
				admissionSQL(t, f, `UPDATE review_operation_waits SET paused=1 WHERE operation_id=?`, r.ID)
				check("operation_paused")
			})
		}
	}
}

func TestSchedulingPausePreservesRetainedCooldown(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var now *time.Time
			if channel == "cli" {
				f, a, _, now = cliWaitFixture(t)
				if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				f, a, _, now = queuedCooldown(t)
			}
			scope := f.scope(t, attemptProject, f.writer, false)
			pause := func(revision int64, paused bool) {
				_, err := f.e.ConfigurePullScheduling(t.Context(), scope, "timer-pause-"+time.Now().Format("150405.000000000"), 2, PullSchedulingRequest{ExpectedRevision: &revision, Paused: &paused})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := cliWaitRead(t, f, a.OperationID)
			pause(0, true)
			if got := f.e.queuedDiagnostic(t.Context(), scope, before.ID, before.Revision); got != "pull_scheduling_paused" {
				t.Fatal("quota/cooldown hid PR pause", got)
			}
			pause(1, false)
			*now = cliWaitDue(t, before).Add(time.Second)
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
				pause(2, true)
				_, err := f.e.ExecutionContext(ctx, o.ProjectID, o.ID)
				return Preparation{}, err
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			after := cliWaitRead(t, f, a.OperationID)
			if driver.prepares.Load() != 1 || driver.calls.Load() != 0 || after.State != "queued" || after.Reason != "pull_scheduling_paused" || !reflect.DeepEqual(after.Wait, before.Wait) {
				t.Fatal("lost original cooldown", before, after)
			}
			assertCLIWaitSnapshot(t, f)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("leaked reservation")
			}
		})
	}
}

func TestSchedulingPreparationGuardCannotRequeueAdmittedAttempt(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed"} {
			t.Run(channel+"/"+reason, func(t *testing.T) {
				f := supervisorFixture(t)
				r := f.enqueue(t, attemptProject, "admitted-guard", channel, 2, 1)
				a := f.start(t, attemptProject, r)
				intent := ""
				if channel == "hosted" {
					intent = "synthetic-trigger"
				}
				admitted, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), intent)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, admitted.Revision, "held", reason, ""); err == nil {
					t.Fatal("admitted attempt accepted a preparation hold")
				}
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, admitted.Revision, "failed", reason, ""); err != nil {
					t.Fatal(err)
				}
				after := cliWaitRead(t, f, r.ID)
				if after.State != "failed" || after.AttemptID != a.ID {
					t.Fatal("admitted action silently requeued", after)
				}
			})
		}
	}
}

func TestSchedulingDetachedGuardHistoryRejectsCorruption(t *testing.T) {
	for _, corrupt := range []string{"anchor", "intent", "external", "state", "revision", "reason"} {
		t.Run(corrupt, func(t *testing.T) {
			f := schedulingFixture(t)
			r := f.enqueue(t, attemptProject, "detached-guard", "hosted", 2, 1)
			a := f.start(t, attemptProject, r)
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "pull_scheduling_paused", ""); err != nil {
				t.Fatal(err)
			}
			assertCLIWaitSnapshot(t, f)
			o := cliWaitRead(t, f, r.ID)
			current, err := readAttempt(t.Context(), f.store.DB(), attemptProject, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch corrupt {
			case "anchor":
				anchor := attemptAnchor(attemptProject, 2)
				current.Anchor = &anchor
			case "intent":
				current.Intent = "possible-post"
			case "external":
				current.ExternalID = "trigger"
			case "state":
				current.State = "active"
			case "revision":
				current.Revision = 1
			case "reason":
				admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='preparation_failed' WHERE attempt_id=? AND revision=2`, a.ID)
			}
			_, recognized, err := historicalUnusedPreparationOperation(t.Context(), f.store.DB(), o, current, true)
			if err == nil && recognized {
				t.Fatal("accepted corrupt detached attempt", corrupt)
			}
		})
	}
}

func TestSchedulingDetachedGuardProjectsOriginalAttempt(t *testing.T) {
	for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed"} {
		t.Run(reason, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			r := f.enqueue(t, attemptProject, "guard-projection", "hosted", 2, 1)
			a := f.start(t, attemptProject, r)
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, ""); err != nil {
				t.Fatal(err)
			}
			_, projected := projectedAttempt(t, f, a)
			if projected.State != "held" || projected.Reason != reason {
				t.Fatal(projected)
			}
			assertCLIWaitSnapshot(t, f)
			o := cliWaitRead(t, f, r.ID)
			if _, err := f.e.ControlOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), "cancel-guard", o.ID, OperationControl{ExpectedRevision: o.Revision, Action: "cancel"}); err != nil {
				t.Fatal(err)
			}
			assertCLIWaitSnapshot(t, f)
			if _, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil); err != nil {
				t.Fatal("historical projection after cancellation", err)
			}
		})
	}
}
