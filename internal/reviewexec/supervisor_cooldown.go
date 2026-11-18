package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"time"
)

// Only unavailable provider evidence permits an operation-local hold. Do not
// turn unrelated database/context errors or a missing attempt into that claim.
func cooldownEvidenceUnavailable(channel string, err error) bool {
	if channel == "cli" {
		return errors.Is(err, errAllowanceObservationAbsent) || storedAllowanceUnavailable(err)
	}
	return channel == "hosted" && (errors.Is(err, errAllowanceObservationAbsent) || storedAllowanceUnavailable(err) || isStoredUnavailable(err, storedHostedObservation))
}

func (e *Engine) persistCooldown(ctx context.Context, original Operation, a Attempt) (Attempt, error) {
	local, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var result Attempt
	err := retryRunSelectionLocal(local, func() error {
		entered := false
		callbackDeadline := false
		writeErr := e.store.BackgroundWrite(local, func(ctx context.Context, tx *sql.Tx) error {
			entered = true
			var err error
			if original.Channel == "cli" {
				result, err = e.scheduleCLICooldown(ctx, tx, a.ProjectID, a.ID, a.Revision)
			} else {
				result, err = e.scheduleHostedCooldown(ctx, tx, a.ProjectID, a.ID, a.Revision)
			}
			// A nonnil callback error makes BackgroundWrite roll back without
			// invoking Commit. Only its actual child deadline is retryable; a
			// domain overload or an expired outer budget is not that proof.
			callbackDeadline = err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && local.Err() == nil
			return err
		})
		if writeErr == nil {
			e.notifyOperation(a.OperationID)
			return nil
		}
		var failure *core.Error
		if (!entered || callbackDeadline) && errors.As(writeErr, &failure) && failure.Code == "overloaded" {
			return writeErr
		}
		// A failed commit may already be present. Only the exact original timer
		// and guarded attempt transition reconcile that ambiguity without a write.
		if recovered, ok := e.recordedCooldown(local, original, a); ok {
			result = recovered
			e.notifyOperation(a.OperationID)
			return nil
		}
		if cooldownEvidenceUnavailable(original.Channel, writeErr) {
			return writeErr
		}
		if errors.As(writeErr, &failure) && failure.Code == "review_cooldown_elapsed" {
			return writeErr
		}
		return persistenceFailure()
	})
	if err != nil {
		if cooldownEvidenceUnavailable(original.Channel, err) {
			return Attempt{}, err
		}
		var failure *core.Error
		if errors.As(err, &failure) && (failure.Code == "review_persistence_failed" || failure.Code == "review_cooldown_elapsed") {
			return Attempt{}, err
		}
		return Attempt{}, persistenceFailure()
	}
	return result, nil
}

func (e *Engine) recordedCooldown(ctx context.Context, original Operation, a Attempt) (Attempt, bool) {
	// A commit error can race supervisor shutdown. Reconcile only the exact
	// durable outcome, with a separate bounded read that survives that caller.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var result Attempt
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := readAttempt(ctx, tx, a.ProjectID, a.ID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, a.ProjectID, a.OperationID)
		if err != nil {
			return err
		}
		baseline := a.ID
		if original.Wait != nil && original.Wait.BaselineAttemptID != "" {
			baseline = original.Wait.BaselineAttemptID
		}
		if current.OperationID != a.OperationID || current.ProjectID != a.ProjectID || current.Provider != a.Provider || current.Account != a.Account || current.Revision != a.Revision+1 || current.State != "held" || current.Anchor != nil || current.Intent != "" || current.ExternalID != "" || o.ID != original.ID || o.ProjectID != original.ProjectID || o.PullNumber != original.PullNumber || o.Channel != original.Channel || o.ActorID != original.ActorID || o.WorkerID != original.WorkerID || o.PolicyRevision != original.PolicyRevision || o.Revision != original.Revision+2 || o.State != "queued" || o.AttemptID != "" || o.Reason != "" || o.HeadSHA != original.HeadSHA || o.ConfigurationRevision != original.ConfigurationRevision || o.CapabilityFingerprint != original.CapabilityFingerprint || o.Wait == nil || o.Wait.BaselineAttemptID != baseline {
			return conflict()
		}
		historical, err := historicalCooldownOperation(ctx, tx, o, current)
		if err != nil {
			return err
		}
		if original.Channel == "cli" {
			record, err := readCLIAllowanceRecord(ctx, tx, historical, current)
			if err != nil || record.Observation.Remaining != 0 {
				return waitUnavailable()
			}
			due, err := cliExhaustionDeadline(ctx, tx, current)
			if err != nil || o.Wait.RetryNotBefore != due.UTC().Format(time.RFC3339Nano) {
				return conflict()
			}
		} else {
			record, err := readHostedAllowanceRecord(ctx, tx, historical, current)
			if err != nil {
				return err
			}
			if record.Input.Observation.State != "cooldown_observed" || o.Wait.RetryNotBefore != record.Input.Observation.RetryNotBefore.UTC().Format(time.RFC3339Nano) {
				return conflict()
			}
		}
		var reserved bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=?)`, a.ID).Scan(&reserved); err != nil {
			return err
		}
		if reserved {
			return conflict()
		}
		result = current
		return nil
	})
	return result, err == nil
}
