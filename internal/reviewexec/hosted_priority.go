package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// earlierQueuedReviewIntent preserves project queue priority. An earlier
// request yields while its own PR cannot start: a source retry, an unresolved
// attempt, or a CLI wait (the account-wide CLI hold is enforced separately).
// Hosted cooldowns keep priority because they reflect the provider account.
// This reserves no provider capacity. Scheduler selection and the serialized
// attempt claim use the same snapshot check; allow_out_of_order concerns
// dependencies, not scheduling priority.
func (e *Engine) earlierQueuedReviewIntent(ctx context.Context, tx *sql.Tx, o Operation, c Configuration, revision int64) (*Operation, error) {
	earlier, err := e.earlierQueuedReviewIntents(ctx, tx, o, c, revision, 1)
	if err != nil || len(earlier) == 0 {
		return nil, err
	}
	return &earlier[0], nil
}

// earlierQueuedReviewIntents returns up to limit eligible predecessors in the
// configured effective order, with at most one operation per PR.
func (e *Engine) earlierQueuedReviewIntents(ctx context.Context, tx *sql.Tx, o Operation, c Configuration, revision int64, limit int) ([]Operation, error) {
	if limit <= 0 || !validChannel(o.Channel) || o.State != "queued" || c.Paused || revision < o.ConfigurationRevision || !slices.Contains(c.Queue, o.PullNumber) {
		return nil, nil
	}
	order, err := orderedConfiguredQueue(ctx, tx, o.ProjectID, c)
	if err != nil {
		return nil, err
	}
	position := slices.Index(order, o.PullNumber)
	if position < 1 {
		return nil, nil
	}
	// An earlier PR that cannot start now (its own wait, retry, pause or
	// unresolved attempt) must not hold later PRs back: PRs are independent.
	now := e.sampleExecutionTime().UTC().Format(time.RFC3339Nano)
	queue, err := json.Marshal(order[:position])
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.id FROM review_operations o
JOIN json_each(?) q ON q.value=o.pull_number
WHERE o.project_id=? AND o.channel=? AND o.state='queued' AND o.reason<>'cli_draft_wait'
AND NOT EXISTS (SELECT 1 FROM review_operation_waits w WHERE w.operation_id=o.id AND (w.paused=1 OR (o.channel='cli' AND w.retry_not_before<>'' AND julianday(w.retry_not_before)>julianday(?))))
AND NOT EXISTS (SELECT 1 FROM review_source_retries sr WHERE sr.operation_id=o.id AND julianday(sr.retry_not_before)>julianday(?))
AND NOT EXISTS (SELECT 1 FROM review_execution_reservations r JOIN projects p ON p.id=o.project_id WHERE r.repository_id=p.github_repository_id AND r.pull_number=o.pull_number AND r.action_class=o.channel)
ORDER BY q.key,o.created_at,o.id`, string(queue), o.ProjectID, o.Channel, now, now)
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
	// An exact empty predecessor set needs no applicability decision. The
	// selected operation still faces all normal guards at actual claim.
	if len(ids) == 0 {
		return nil, nil
	}
	// Inapplicable requests must still reach normal StartAttempt hold handling,
	// rather than being hidden behind another intent indefinitely.
	valid, err := e.hostedPriorityBinding(ctx, tx, o)
	if err != nil || !valid {
		return nil, err
	}
	var result []Operation
	seen := make(map[int64]bool)
	for _, id := range ids {
		earlier, err := readOperation(ctx, tx, o.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if seen[earlier.PullNumber] {
			continue
		}
		valid, err := e.hostedPriorityBinding(ctx, tx, earlier)
		if err != nil {
			return nil, err
		}
		if valid {
			result = append(result, earlier)
			seen[earlier.PullNumber] = true
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (e *Engine) hostedPriorityBinding(ctx context.Context, tx *sql.Tx, o Operation) (bool, error) {
	err := requireExternalHostedSettled(ctx, tx, o.ProjectID, o.PullNumber, o.Channel)
	if err == nil {
		_, err = e.currentExecutionContext(ctx, tx, o)
	}
	if err == nil {
		return true, nil
	}
	var refusal *core.Error
	if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
		return false, nil
	}
	return false, err
}
