package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
)

// earlierQueuedHostedIntent preserves project queue priority even while the
// earlier request's timer is not due. It reserves no provider capacity. Both
// scheduler selection and the serialized attempt claim use this same snapshot
// check; allow_out_of_order concerns dependencies, not scheduling priority.
func (e *Engine) earlierQueuedHostedIntent(ctx context.Context, tx *sql.Tx, o Operation, c Configuration, revision int64) (*Operation, error) {
	position := slices.Index(c.Queue, o.PullNumber)
	if o.Channel != "hosted" || o.State != "queued" || position < 1 || c.Paused || revision < o.ConfigurationRevision {
		return nil, nil
	}
	// Inapplicable requests must still reach normal StartAttempt hold handling,
	// rather than being hidden behind another intent indefinitely.
	valid, err := e.hostedPriorityBinding(ctx, tx, o)
	if err != nil || !valid {
		return nil, err
	}
	queue, err := json.Marshal(c.Queue)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.id FROM review_operations o
JOIN json_each(?) q ON q.value=o.pull_number AND q.key<?
WHERE o.project_id=? AND o.channel='hosted' AND o.state='queued'
AND NOT EXISTS (SELECT 1 FROM review_operation_waits w WHERE w.operation_id=o.id AND w.paused=1)
ORDER BY q.key,o.created_at,o.id`, string(queue), position, o.ProjectID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		earlier, err := readOperation(ctx, tx, o.ProjectID, id)
		if err != nil {
			return nil, err
		}
		valid, err := e.hostedPriorityBinding(ctx, tx, earlier)
		if err != nil {
			return nil, err
		}
		if valid {
			return &earlier, nil
		}
	}
	return nil, nil
}

func (e *Engine) hostedPriorityBinding(ctx context.Context, tx *sql.Tx, o Operation) (bool, error) {
	_, err := e.currentExecutionContext(ctx, tx, o)
	if err == nil {
		return true, nil
	}
	var refusal *core.Error
	if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
		return false, nil
	}
	return false, err
}
