package reviewexec

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/reviews"
	"slices"
)

// Queue sorting is independent of stacked/independent source admission. Empty
// retained configuration means Auto for backwards compatibility. Manual returns
// saved membership/order, not permission to execute any member.
func queueSortMode(c Configuration) string {
	if c.QueueSortMode == "" {
		return "auto"
	}
	return c.QueueSortMode
}
func orderedConfiguredQueue(ctx context.Context, tx *sql.Tx, project string, c Configuration) ([]int64, error) {
	switch queueSortMode(c) {
	case "manual":
		return slices.Clone(c.Queue), nil
	case "auto":
		return reviews.OrderedExecutionQueue(ctx, tx, project, c.Queue)
	default:
		return nil, invalid()
	}
}
