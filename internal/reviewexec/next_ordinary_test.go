package reviewexec

import (
	"context"
	"database/sql"
	"testing"
)

func TestQueuedReservationRequiresCurrentCapability(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			holder := f.enqueue(t, attemptProject, "diagnostic-holder", channel, 2, 1)
			f.start(t, attemptProject, holder)
			waiting := f.enqueue(t, attemptProject, "diagnostic-waiter", channel, 4, 1)
			op := roundOperation(t, f, waiting.ID)
			scope := f.scope(t, attemptProject, f.writer, false)
			cause, reservation := f.e.queuedDiagnosticDetails(t.Context(), scope, op.ID, op.Revision)
			if cause != "reservation_busy" || reservation == nil || reservation.OperationID != holder.ID {
				t.Fatalf("compatible reservation omitted: %q %+v", cause, reservation)
			}
			f.configure(t, attemptProject, "other-account", 1, false)
			cause, reservation = f.e.queuedDiagnosticDetails(t.Context(), scope, op.ID, op.Revision)
			if cause == "reservation_busy" || reservation != nil {
				t.Fatalf("obsolete capability represented as current reservation: %q %+v", cause, reservation)
			}
		})
	}
}

func TestAutomaticSourceRetryTwoNominatedPlansReachWriter(t *testing.T) {
	f, original := sourceRetryFixture(t)
	ageSourceRetry(t, f, original.ID)
	readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
	var plans [2]*automaticProjectPlan
	for i := range plans {
		if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			var err error
			plans[i], err = f.e.planAutomaticProject(ctx, tx, original.ProjectID, original.Channel, readers)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if plan := plans[i]; plan == nil || plan.retry == nil || plan.retry.ID != original.ID || plan.retry.Revision != original.Revision {
			t.Fatalf("snapshot %d did not independently nominate the same retained retry: %+v", i, plan)
		}
	}
	for i, plan := range plans {
		wrote, err := f.e.acceptAutomaticProject(t.Context(), original.ProjectID, original.Channel, readers, plan)
		if err != nil || wrote != (i == 0) {
			t.Fatalf("writer %d committed=%t, err=%v", i, wrote, err)
		}
	}
	got := selectionRead(t, f, original.ID)
	if got.State != "queued" || got.Revision != original.Revision+1 || got.OperationID != "" ||
		attemptCount(t, f.store, "SELECT count(*) FROM review_selections") != 1 ||
		attemptCount(t, f.store, "SELECT count(*) FROM review_operations") != 0 {
		t.Fatalf("writer changed retry identity, revision or child state: %+v", got)
	}
}
