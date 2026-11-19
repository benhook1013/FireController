package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
)

// A validated source frame is invariant inside one planning transaction. The
// memo is scoped to that plan, never stored on Engine or reused by its writer.
type automaticFrameKey struct{}
type automaticFrameSnapshot struct {
	project, channel string
	frame            frontFrame
	found            bool
	err              error
}

func automaticDependencyFrame(ctx context.Context, tx *sql.Tx, project, channel string) (frontFrame, bool, error) {
	if memo, ok := ctx.Value(automaticFrameKey{}).(automaticFrameSnapshot); ok && memo.project == project && memo.channel == channel {
		return memo.frame, memo.found, memo.err
	}
	return readFrontFrame(ctx, tx, project, channel)
}

// Automatic planning reads local retained state, not GitHub. In particular,
// overloaded can mean a local deadline OR contention; do not invent a provider
// failure or claim which local cause was discarded by storage normalization.
func automaticPlanErrorReason(err error) string {
	if automaticProjectRefused(err) {
		return "local_planning_state_invalid"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "local_planning_deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "local_planning_cancelled"
	}
	var failure *core.Error
	if errors.As(err, &failure) {
		switch failure.Code {
		case "overloaded":
			return "local_planning_overloaded"
		case "storage_unavailable":
			return "local_planning_storage_unavailable"
		case "automation_authorization_changed", "forbidden":
			return "local_planning_authorization_changed"
		}
	}
	return "local_planning_failed"
}
