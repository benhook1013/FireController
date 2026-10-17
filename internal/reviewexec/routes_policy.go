package reviewexec

import (
	"context"
	"database/sql"
)

// Incoming routes are independent finding obligations, not review captures.
// Dirty receiving-scope authority refuses the assessment rather than passing
// zero obligations. The boolean preserves the policy response shape.
func incomingRoutePolicyFacts(ctx context.Context, tx *sql.Tx, project string, pull int64) (int64, bool, error) {
	pending, err := pendingIncomingRoutes(ctx, tx, project, pull)
	return pending, false, err
}
