package reviewexec

import (
	"context"
	"database/sql"
	"slices"
)

// pausedRunSuffix proves only that no unseen entry can be selected now. It
// never supplies source facts, completion credit or permission to select a PR.
// An unseen parent might complete a previously blocked candidate, so those
// dependencies must still be acquired normally, even when the parent is paused.
func pausedRunSuffix(ctx context.Context, tx *sql.Tx, project string, prefix []SourcePlan, suffix []int64) (bool, error) {
	if len(suffix) == 0 {
		return false, nil
	}
	for _, plan := range prefix {
		if slices.Contains(suffix, plan.ParentPullNumber) {
			return false, nil
		}
	}
	for _, number := range suffix {
		scheduling, err := readPullScheduling(ctx, tx, project, number)
		if err != nil {
			return false, err
		}
		if !scheduling.Paused {
			return false, nil
		}
	}
	return true, nil
}
