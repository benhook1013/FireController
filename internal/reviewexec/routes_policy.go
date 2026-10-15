package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
)

// Incoming routes are independent finding obligations, not review captures.
// A bounded-inspection refusal is an explicit hold rather than zero obligations.
func incomingRoutePolicyFacts(ctx context.Context, tx *sql.Tx, project string, pull int64) (int64, bool, error) {
	pending, err := pendingIncomingRoutes(ctx, tx, project, pull)
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "review_capacity" {
		return 0, true, nil
	}
	return pending, false, err
}
