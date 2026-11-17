package reviewexec

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownRetainsCommittedCLIExhaustionForRecovery(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-persistence", true: "during-persistence"}[during], func(t *testing.T) {
			f := supervisorFixture(t)
			request := f.enqueue(t, attemptProject, "shutdown-exhaustion", "cli", 2, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var retained atomic.Bool
			f.e.schedulingNow = func() time.Time {
				if during && retained.Load() {
					cancel()
				}
				return time.Now().UTC()
			}
			d := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
				in := CLIAllowanceObservation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber), ObservedAt: stamp(), Remaining: 0, Limit: 5, RollingWindowMS: 3600000, ChangedFiles: 2}
				if err := f.e.RecordCLIExhaustion(ctx, o.ProjectID, a.ID, a.Revision, in); err != nil {
					return Preparation{}, err
				}
				retained.Store(true)
				if !during {
					cancel()
				}
				return Preparation{}, sourceHold("allowance_exhausted")
			}}
			err := f.e.processNext(ctx, map[string]Driver{"test-capability": d})
			var recovery *PreparationRecoveryError
			if !errors.As(err, &recovery) {
				t.Fatal("shutdown failed to preserve recovery requirement", err)
			}
			o := cliWaitRead(t, f, request.ID)
			if o.State != "preparing" || o.AttemptID == "" || d.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
				t.Fatal("committed quota proof lost preparing ownership", o)
			}
			before := snapshotTable(t, f, "review_cli_preflight")
			// Recovery needs no provider read, reuses the operation, and restores shared-account backoff.
			f.e.schedulingNow = nil
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			o = cliWaitRead(t, f, request.ID)
			due := cliWaitDue(t, o)
			if !due.After(time.Now()) || before != snapshotTable(t, f, "review_cli_preflight") || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("recovery lost original evidence or quota floor")
			}
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			other := f.enqueue(t, otherAttemptProject, "same-account", "cli", 2, 1)
			a, err := f.e.StartAttempt(t.Context(), otherAttemptProject, other.ID)
			if err != nil || a.ID != "" {
				t.Fatal("same-account retry bypassed restored floor", a, err)
			}
		})
	}
}
