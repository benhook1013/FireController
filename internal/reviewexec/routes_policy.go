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

// An unassigned route is not a review-admission hold, but must never disappear
// from combined readiness or imply owner approval to merge.
func pendingUnassignedSourceRoutes(ctx context.Context, tx *sql.Tx, project string, pull int64) (int64, error) {
	var dirty bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM review_route_policy_invalidations WHERE project_id=? AND target_pull_number=0)", project).Scan(&dirty); err != nil {
		return 0, err
	}
	if dirty {
		return 0, routeStorageError()
	}
	var count int64
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM review_route_policy_facts f JOIN review_finding_routes r ON r.id=f.route_id AND r.project_id=f.project_id WHERE f.project_id=? AND r.source_pull_number=? AND f.target_pull_number=0 AND f.active=1 AND f.pending=1", project, pull).Scan(&count)
	return count, err
}
