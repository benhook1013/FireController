package reviewexec

import (
	"context"
	"log/slog"
)

// PreparationRecoveryError leaves an undispatched attempt preparing after a
// local retention failure. Startup must reconcile its original private source
// before ordinary interrupted-attempt recovery can release its reservation.
type PreparationRecoveryError struct{ Err error }

func (e *PreparationRecoveryError) Error() string {
	return "Review preparation retention requires recovery."
}
func (e *PreparationRecoveryError) Unwrap() error { return e.Err }

type preparationRecoveryDriver interface {
	RecoverPreparation(context.Context, Operation, Attempt) error
}

func (e *Engine) reconcilePreparingDrivers(ctx context.Context, drivers map[string]Driver) (map[string]bool, error) {
	skipped := map[string]bool{}
	if err := e.reconcileCLIExhaustions(ctx, skipped); err != nil {
		return nil, err
	}
	rows, err := e.store.DB().QueryContext(ctx, `SELECT a.project_id,a.id,a.operation_id,
 EXISTS(SELECT 1 FROM review_hosted_allowance_queries q WHERE q.attempt_id=a.id)
 OR EXISTS(SELECT 1 FROM review_hosted_observations h WHERE h.attempt_id=a.id)
 FROM review_execution_attempts a
 JOIN review_operations o ON o.id=a.operation_id AND o.attempt_id=a.id
 WHERE a.state='preparing' AND o.state='preparing' AND o.channel='hosted'
 AND NOT EXISTS (SELECT 1 FROM review_hosted_preflight p WHERE p.attempt_id=a.id) ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	type identity struct {
		project, attempt, operation string
		hasRecoveryEvidence         bool
	}
	var ids []identity
	for rows.Next() {
		var id identity
		if err = rows.Scan(&id.project, &id.attempt, &id.operation, &id.hasRecoveryEvidence); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		a, err := readAttempt(ctx, e.store.DB(), id.project, id.attempt)
		if err != nil {
			if isStoredUnavailable(err, storedReviewAdmission) {
				skipped[id.attempt] = true
				slog.WarnContext(ctx, "Hosted preparation attempt is unavailable", "attempt_id", id.attempt, "project_id", id.project)
				continue
			}
			return nil, err
		}
		o, err := readOperationIdentity(ctx, e.store.DB(), id.project, id.operation)
		if err != nil {
			if isStoredUnavailable(err, storedOperationIdentity) || isRetainedOperationDecodeError(err) {
				skipped[id.attempt] = true
				slog.WarnContext(ctx, "Hosted preparation identity is unavailable", "operation_id", id.operation, "attempt_id", id.attempt)
				continue
			}
			return nil, err
		}
		o.Wait, err = readOperationWait(ctx, e.store.DB(), o)
		if err != nil {
			if !isWaitUnavailable(err) {
				return nil, err
			}
			skipped[id.attempt] = true
			slog.WarnContext(ctx, "Hosted preparation wait is unavailable", "operation_id", id.operation, "attempt_id", id.attempt)
			continue
		}
		var replayed bool
		for key, capability := range e.capabilities {
			if capability.Fingerprint != o.CapabilityFingerprint || capability.Provider != a.Provider || capability.Account != a.Account {
				continue
			}
			if driver, ok := drivers[key].(preparationRecoveryDriver); ok {
				replayed = true
				if err := driver.RecoverPreparation(ctx, o, a); err != nil {
					return nil, err
				}
			}
			break
		}
		if !replayed && id.hasRecoveryEvidence {
			// The ordinary interruption pass would cancel this undispatched
			// attempt before its original cooldown could be recovered.
			skipped[id.attempt] = true
			slog.WarnContext(ctx, "Hosted preparation recovery requires its original capability", "operation_id", id.operation, "attempt_id", id.attempt, "project_id", id.project, "reason", "review_preparation_recovery_unavailable")
		}
	}
	return skipped, nil
}
