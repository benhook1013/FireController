package reviewexec

import (
	"context"
	"database/sql"
)

// RoundAdmission reads the operation-bound local grant before source/workspace
// work. It is not provider authority; final admission repeats the guard.
func (e *Engine) RoundAdmission(ctx context.Context, project, operationID string) (bool, error) {
	if !identifier.MatchString(project) || !identifier.MatchString(operationID) {
		return false, invalid()
	}
	var configured bool
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		operation, err := readOperation(ctx, tx, project, operationID)
		if err != nil {
			return err
		}
		if err := requireSourceObligationsClear(ctx, tx, project, operation.PullNumber); err != nil {
			return err
		}
		configured, err = admitRoundControl(ctx, tx, operation)
		return err
	})
	return configured, err
}

// DiscoveryComplete controls channel progression, independently of whether a
// stopped comparison has current captured coverage for merge readiness.
func (e *Engine) DiscoveryComplete(ctx context.Context, project string, current SourcePlan, channel string) (bool, error) {
	state, err := e.Comparison(ctx, project, current, channel)
	if err != nil {
		return false, err
	}
	if state.Pending {
		return false, nil
	}
	if state.RoundsConfigured {
		return state.RoundsComplete, nil
	}
	return state.Covered, nil
}

// Only these local grant refusals are safe to retain as preparation holds.
func roundControlRefusal(code string) bool {
	switch code {
	case "review_round_closed", "review_round_complete", "review_round_changed", "review_round_capacity":
		return true
	}
	return false
}
