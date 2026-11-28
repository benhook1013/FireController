package reviewexec

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLiveCLIPreparationRefusalSettlesOriginalClaim(t *testing.T) {
	for _, existingAllowance := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-usage", true: "existing-allowance"}[existingAllowance], func(t *testing.T) {
			f := supervisorFixture(t)
			request := f.enqueue(t, attemptProject, "live-preparation-refusal", "cli", 2, 1)
			released := false
			d := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
				if existingAllowance {
					in := CLIAllowanceObservation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber), ObservedAt: stamp(), Remaining: 2, Limit: 5, RollingWindowMS: 3600000, ChangedFiles: 2}
					if err := f.e.RecordCLIAllowance(ctx, o.ProjectID, a.ID, a.Revision, in); err != nil {
						t.Fatal(err)
					}
				}
				return Preparation{}, CLIRefusalBeforeUsage(newStoredUnavailable(storedRoundHistory, "Synthetic round history refusal."), func() { released = true })
			}}
			drivers := map[string]Driver{"test-capability": d}
			err := f.e.processNext(t.Context(), drivers)
			op := roundOperation(t, f, request.ID)
			reservations := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`)
			if d.prepares.Load() != 1 || d.calls.Load() != 0 || released != !existingAllowance {
				t.Fatal("preparation refusal caused additional preparation or dispatch")
			}
			if existingAllowance {
				var recovery *PreparationRecoveryError
				if !errors.As(err, &recovery) || op.State != "preparing" || op.AttemptID == "" || reservations != 1 {
					t.Fatal("existing allowance lost custody", op, reservations, err)
				}
				return
			}
			if err != nil || op.State != "queued" || op.Reason != "automatic_scheduling_unavailable" || op.AttemptID != "" || reservations != 0 {
				t.Fatal("unused preparation failed to release the original reservation", op, reservations, err)
			}
			d.prepare = func(_ context.Context, _ Operation, _ Configuration, _ Attempt) (Preparation, error) {
				return Preparation{}, sourceHold("allowance_unavailable")
			}
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			if op = roundOperation(t, f, request.ID); d.prepares.Load() != 2 || d.calls.Load() != 0 || op.State != "held" || op.Reason != "allowance_unavailable" {
				t.Fatal("running supervisor did not resume the original request", op)
			}
		})
	}
}

type liveCLIExhaustionSource struct {
	record CLIExhaustionRecovery
	found  bool
}

func (source liveCLIExhaustionSource) LoadCLIExhaustion(context.Context, string) (CLIExhaustionRecovery, bool, error) {
	return source.record, source.found, nil
}

func TestLiveCLIPreparationReplaysOnlyRetainedExactZero(t *testing.T) {
	for _, found := range []bool{true, false} {
		t.Run(map[bool]string{true: "retained-zero", false: "missing-capture"}[found], func(t *testing.T) {
			f := supervisorFixture(t)
			request := f.enqueue(t, attemptProject, "live-preparation-exhaustion", "cli", 2, 1)
			var originalAttempt string
			released := false
			d := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
				originalAttempt = a.ID
				record := CLIExhaustionRecovery{
					Operation:   roundOperation(t, f, o.ID),
					Attempt:     a,
					Observation: CLIAllowanceObservation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber), ObservedAt: stamp(), Remaining: 0, Limit: 5, RollingWindowMS: 3600000, ChangedFiles: 2},
				}
				return Preparation{}, RetainedCLIExhaustionHandoff(errors.New("synthetic journal write unavailable"), liveCLIExhaustionSource{record: record, found: found}, func() { released = true })
			}}
			err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d})
			op := roundOperation(t, f, request.ID)
			journaled := attemptCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`)
			reserved := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`)
			if d.prepares.Load() != 1 || d.calls.Load() != 0 || !released {
				t.Fatal("local replay performed provider work")
			}
			if !found {
				var recovery *PreparationRecoveryError
				if !errors.As(err, &recovery) || op.State != "preparing" || op.AttemptID != originalAttempt || reserved != 1 || journaled != 0 {
					t.Fatal("missing capture lost custody", op, reserved, journaled, err)
				}
				return
			}
			if err != nil || op.State != "queued" || op.AttemptID != "" || op.Wait == nil || op.Wait.BaselineAttemptID != originalAttempt ||
				!cliWaitDue(t, op).After(time.Now()) || reserved != 0 || journaled != 1 {
				t.Fatal("exact zero did not preserve the original retry baseline and backoff", op, reserved, journaled, err)
			}
		})
	}
}

func TestLiveCLIPreparationRetainedHandoffTransfersToSameEngineRecovery(t *testing.T) {
	f := supervisorFixture(t)
	request := f.enqueue(t, attemptProject, "retained-handoff-transfers", "cli", 2, 1)
	var originalAttempt string
	loads := 0
	released := false
	driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
		originalAttempt = a.ID
		record := CLIExhaustionRecovery{
			Operation:   roundOperation(t, f, o.ID),
			Attempt:     a,
			Observation: CLIAllowanceObservation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber), ObservedAt: stamp(), Remaining: 0, Limit: 5, RollingWindowMS: 3600000, ChangedFiles: 2},
		}
		source := exhaustionRecoverySourceFunc(func(_ context.Context, id string) (CLIExhaustionRecovery, bool, error) {
			loads++
			if id != a.ID {
				t.Fatalf("recovery requested a different attempt: %s", id)
			}
			if loads == 1 {
				return CLIExhaustionRecovery{}, false, errors.New("temporary local capture read failure")
			}
			if !released {
				t.Fatal("startup replay did not receive ownership")
			}
			return record, true, nil
		})
		f.e.cliExhaustionRecovery = source
		return Preparation{}, RetainedCLIExhaustionHandoff(errors.New("temporary local journal failure"), source, func() { released = true })
	}}
	drivers := map[string]Driver{"test-capability": driver}
	var recoverable *PreparationRecoveryError
	if err := f.e.processNext(t.Context(), drivers); !errors.As(err, &recoverable) || !released {
		t.Fatal("transient settlement did not hand durable recovery off", err)
	}
	pending := roundOperation(t, f, request.ID)
	if pending.State != "preparing" || pending.AttemptID != originalAttempt {
		t.Fatal("failed local settlement changed the accepted attempt", pending)
	}
	skipped, err := f.e.reconcilePreparingDrivers(t.Context(), drivers)
	if err != nil || len(skipped) != 0 {
		t.Fatal("same engine startup could not recover durable handoff", skipped, err)
	}
	got := roundOperation(t, f, request.ID)
	if loads != 2 || driver.prepares.Load() != 1 || driver.calls.Load() != 0 ||
		got.State != "queued" || got.AttemptID != "" || got.Wait == nil || got.Wait.BaselineAttemptID != originalAttempt ||
		!cliWaitDue(t, got).After(time.Now()) {
		t.Fatal("startup recovery did not settle the sole original attempt", got)
	}
}
