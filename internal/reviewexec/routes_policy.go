package reviewexec

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/core"
)

// Receiving obligations belong to the target regardless of queue scheduling.
// The indexed owner refuses dirty authority instead of treating it as empty.
func requireIncomingRoutesClear(ctx context.Context, tx *sql.Tx, project string, pull int64) error {
	pending, err := pendingIncomingRoutes(ctx, tx, project, pull)
	if err != nil {
		return err
	}
	if pending > 0 {
		return &core.Error{Code: "review_held", Message: "Resolve incoming routed findings before requesting another review.", Status: 409, Details: map[string]string{"reason": "routed_findings_pending"}}
	}
	return nil
}

// Incoming routes are independent finding obligations, not review captures.
// Dirty receiving-scope authority refuses the assessment rather than passing
// zero obligations. The boolean preserves the policy response shape.
func incomingRoutePolicyFacts(ctx context.Context, tx *sql.Tx, project string, pull int64) (int64, bool, error) {
	pending, err := pendingIncomingRoutes(ctx, tx, project, pull)
	return pending, false, err
}
