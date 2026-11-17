package reviewexec

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func holdAutomaticCLISource(t *testing.T, f *attemptFixture, o Operation, reason string) Attempt {
	t.Helper()
	a := f.start(t, attemptProject, Receipt{ID: o.ID})
	held, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", reason, "")
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func TestAutomaticCLITransientSourceRetryPreservesQueueAndHistory(t *testing.T) {
	for _, reason := range []string{transientCLISourceHold, legacyCLISourceHold} {
		t.Run(reason, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingCLIEnable(t, f, 0, true)
			schedulingCLIGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			run := schedulingLatest(t, f)
			front := cliWaitRead(t, f, run.OperationID)
			if front.PullNumber != 2 {
				t.Fatal(front)
			}
			held := holdAutomaticCLISource(t, f, front, reason)
			stopped := cliWaitRead(t, f, front.ID)
			heldAt, _ := attemptRecordedTime(stopped.UpdatedAt)
			now := heldAt.Add(transientSourceDelay - time.Nanosecond)
			f.e.schedulingNow = func() time.Time { return now }

			// A later PR can acquire its own request while the front is held.
			schedulingCLIGenerate(t, f, true)
			selectionNext(t, f, &refreshTestReader{})
			successor := cliWaitRead(t, f, schedulingLatest(t, f).OperationID)
			if successor.PullNumber != 4 || successor.State != "queued" {
				t.Fatal(successor)
			}
			schedulingCLIGenerate(t, f, false)
			now = heldAt.Add(transientSourceDelay)
			if reason == legacyCLISourceHold {
				schedulingCLIGenerate(t, f, false)
				refresh := refreshEnqueue(t, f, "exact-source-recovered", refreshRequest())
				worked, err := f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
				if err != nil || !worked || refreshRead(t, f, refresh.ID).State != "completed" {
					t.Fatal(worked, err)
				}
			}

			schedulingCLIGenerate(t, f, true)
			retried := cliWaitRead(t, f, front.ID)
			if retried.State != "queued" || retried.Revision != stopped.Revision+1 || retried.AttemptID != "" ||
				retried.CreatedAt != front.CreatedAt || !reflect.DeepEqual(successor, cliWaitRead(t, f, successor.ID)) ||
				!reflect.DeepEqual(run, selectionRead(t, f, run.ID)) {
				t.Fatal(retried)
			}
			if n := attemptCount(t, f.store, "SELECT count(*) FROM review_operations"); n != 2 {
				t.Fatal("retry created another operation", n)
			}
			if n := attemptCount(t, f.store, "SELECT count(*) FROM review_execution_attempts"); n != 1 {
				t.Fatal("scheduling retry started an attempt", n)
			}
			old, err := readAttempt(t.Context(), f.store.DB(), attemptProject, held.ID)
			if err != nil || !reflect.DeepEqual(held, old) {
				t.Fatal(old, err)
			}
			if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				return ValidateOperationWaitsSnapshot(ctx, tx)
			}); err != nil {
				t.Fatal("historical attempt failed backup validation", err)
			}
			a, err := f.e.StartAttempt(t.Context(), attemptProject, successor.ID)
			if err != nil || a.ID != "" {
				t.Fatal("successor bypassed earlier eligible request", a, err)
			}

			heldAgain := holdAutomaticCLISource(t, f, retried, transientCLISourceHold)
			now = now.Add(2 * transientSourceDelay)
			schedulingCLIGenerate(t, f, false)
			if final := cliWaitRead(t, f, front.ID); final.AttemptID != heldAgain.ID || final.State != "held" {
				t.Fatal("retry cap did not hold", final)
			}
		})
	}
}

func TestAutomaticCLITransientSourceRetryRefusesAttemptWithAllowance(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{})
	o := cliWaitRead(t, f, schedulingLatest(t, f).OperationID)
	a := f.start(t, attemptProject, Receipt{ID: o.ID})
	input := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", transientCLISourceHold, ""); err != nil {
		t.Fatal(err)
	}
	f.e.schedulingNow = func() time.Time { return time.Now().Add(time.Minute) }
	schedulingCLIGenerate(t, f, false)
}
