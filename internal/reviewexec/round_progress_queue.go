package reviewexec

import (
	"context"
	"database/sql"
)

const reviewQueueDetailsHeader = "X-FC-Review-Queue-Details"
const maxVisibleQueuePredecessors = 5

// QueuedReviewPosition reports accepted predecessors in effective order
// and this project's known reservations when the relevant capacity is busy.
type QueuedReviewPosition struct {
	Ahead   []int64 `json:"ahead,omitempty"`
	Holding []int64 `json:"holding,omitempty"`
	More    bool    `json:"more,omitempty"`
}

// This is optional local diagnostic evidence. If a binding cannot be verified,
// preserve the existing wait and progress facts without adding a position.
func (e *Engine) readQueuedReviewPosition(ctx context.Context, tx *sql.Tx, project string, pull int64, channel string, c Configuration, revision int64) *QueuedReviewPosition {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM review_operations
		WHERE project_id=? AND pull_number=? AND channel=? AND state='queued'
		  AND attempt_id='' AND reason<>'cli_draft_wait'
		ORDER BY created_at,id LIMIT 1`, project, pull, channel).Scan(&id)
	if err != nil {
		return nil
	}
	operation, err := readOperationBase(ctx, tx, project, id)
	if err != nil || operation.Wait != nil && operation.Wait.Paused {
		return nil
	}
	if operation.Wait != nil && operation.Wait.SourceRetry != nil && e.sampleExecutionTime().Before(mustWaitTime(operation.Wait.SourceRetry.RetryNotBefore)) {
		return nil
	}
	valid, err := e.hostedPriorityBinding(ctx, tx, operation)
	if err != nil || !valid {
		return nil
	}
	position := &QueuedReviewPosition{}
	if earlier, err := e.earlierQueuedReviewIntents(ctx, tx, operation, c, revision, maxVisibleQueuePredecessors+1); err == nil {
		position.More = len(earlier) > maxVisibleQueuePredecessors
		for _, item := range earlier[:min(len(earlier), maxVisibleQueuePredecessors)] {
			position.Ahead = append(position.Ahead, item.PullNumber)
		}
	}
	position.Holding = e.readQueueSlotHolders(ctx, tx, operation, e.capabilities[c.CapabilityID])
	if len(position.Ahead)+len(position.Holding) == 0 {
		return nil
	}
	return position
}

func (e *Engine) readQueueSlotHolders(ctx context.Context, tx *sql.Tx, operation Operation, capability Capability) []int64 {
	if operation.Channel == "cli" {
		free, err := e.cliAccountFree(ctx, tx, capability.Provider, capability.Account)
		if err != nil || free > 0 {
			return nil
		}
	}
	// Attribute only reservations that belong to this project. The CLI
	// account limit may also be shared with projects outside this read.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT r.pull_number
		FROM review_execution_reservations r
		JOIN review_execution_attempts a ON a.id=r.attempt_id AND a.project_id=?
		JOIN projects p ON p.id=a.project_id AND p.github_repository_id=r.repository_id
		WHERE r.action_class=? AND r.provider=? AND r.account=? AND r.execution_ended_at=''
		  AND r.pull_number<>?
		ORDER BY r.pull_number LIMIT 16`, operation.ProjectID, operation.Channel, capability.Provider, capability.Account, operation.PullNumber)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var pulls []int64
	for rows.Next() {
		var pull int64
		if rows.Scan(&pull) != nil || pull < 1 || pull > 2147483647 {
			return nil
		}
		pulls = append(pulls, pull)
	}
	if rows.Err() != nil {
		return nil
	}
	return pulls
}
