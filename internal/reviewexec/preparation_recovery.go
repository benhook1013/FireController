package reviewexec

import (
	"context"

	"github.com/benhook1013/FireController/internal/core"
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

func (e *Engine) reconcilePreparingDrivers(ctx context.Context, drivers map[string]Driver) error {
	rows, err := e.store.DB().QueryContext(ctx, `SELECT a.project_id,a.id,a.operation_id,
 EXISTS(SELECT 1 FROM review_hosted_allowance_queries q WHERE q.attempt_id=a.id)
 AND EXISTS(SELECT 1 FROM review_hosted_observations h WHERE h.attempt_id=a.id)
 FROM review_execution_attempts a
 JOIN review_operations o ON o.id=a.operation_id AND o.attempt_id=a.id
 WHERE a.state='preparing' AND o.state='preparing' AND o.channel='hosted'
 AND NOT EXISTS (SELECT 1 FROM review_hosted_preflight p WHERE p.attempt_id=a.id) ORDER BY a.id`)
	if err != nil {
		return err
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
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		a, err := readAttempt(ctx, e.store.DB(), id.project, id.attempt)
		if err != nil {
			return err
		}
		o, err := readOperationBase(ctx, e.store.DB(), id.project, id.operation)
		if err != nil {
			return err
		}
		var replayed bool
		for key, capability := range e.capabilities {
			if capability.Fingerprint != o.CapabilityFingerprint || capability.Provider != a.Provider || capability.Account != a.Account {
				continue
			}
			if driver, ok := drivers[key].(preparationRecoveryDriver); ok {
				replayed = true
				if err := driver.RecoverPreparation(ctx, o, a); err != nil {
					return err
				}
			}
			break
		}
		if !replayed && id.hasRecoveryEvidence {
			// The ordinary interruption pass would cancel this undispatched
			// attempt before its original cooldown could be recovered.
			return &core.Error{
				Code: "review_preparation_recovery_unavailable", Status: 503,
				Message: "Restore the original Hosted capability to recover its retained preparation.",
				Details: map[string]string{"operation_id": id.operation, "attempt_id": id.attempt},
			}
		}
	}
	return nil
}
