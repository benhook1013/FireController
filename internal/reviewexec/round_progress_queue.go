package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/benhook1013/FireController/internal/reviews"
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

// HostedLaneHold identifies the same-project reservation that is still holding
// the configured Hosted account's repository lane. It does not prove activity.
type HostedLaneHold struct {
	PullNumber int64                  `json:"pull_number"`
	Attempt    reviews.AttemptSummary `json:"attempt"`
}

func (e *Engine) readHostedLaneHold(ctx context.Context, tx *sql.Tx, project, capabilityID string) (*HostedLaneHold, error) {
	capability, ok := e.capabilities[capabilityID]
	if !ok || !slices.Contains(capability.Channels, "hosted") {
		return nil, nil
	}
	var hold HostedLaneHold
	var repositoryID int64
	var reason, anchorJSON, transitionedAt string
	attempt := &hold.Attempt
	// The Hosted account/repository uniqueness constraint bounds this lookup.
	// Compare with the admitted head, which may differ from the current PR head.
	err := tx.QueryRowContext(ctx, `SELECT r.pull_number,r.repository_id,a.id,o.channel,o.head_sha,a.state,
		first.created_at,current.created_at,o.reason,a.external_id,
		CASE WHEN length(CAST(a.anchor_json AS BLOB))<=4096 THEN a.anchor_json ELSE '' END
	FROM projects p
	JOIN review_execution_reservations r ON r.repository_id=p.github_repository_id
	JOIN review_execution_attempts a ON a.id=r.attempt_id AND a.project_id=p.id AND a.provider=r.provider AND a.account=r.account
	JOIN review_operations o ON o.id=a.operation_id AND o.project_id=a.project_id AND o.attempt_id=a.id
		AND o.pull_number=r.pull_number AND o.channel=r.action_class AND o.state=a.state
	JOIN review_execution_transitions first ON first.attempt_id=a.id AND first.revision=1 AND first.state='preparing'
	JOIN review_execution_transitions current ON current.attempt_id=a.id AND current.revision=a.revision
		AND current.state=a.state AND current.reason=o.reason AND current.external_id=a.external_id
	WHERE p.id=? AND r.provider=? AND r.account=? AND r.action_class='hosted' AND r.execution_ended_at=''
		AND o.capability_fingerprint=? AND a.state IN ('active','uncertain')`, project, capability.Provider, capability.Account, capability.Fingerprint).
		Scan(&hold.PullNumber, &repositoryID, &attempt.SourceAttemptID, &attempt.Channel, &attempt.HeadSHA, &attempt.State,
			&attempt.StartedAt, &transitionedAt, &reason, &attempt.TriggerID, &anchorJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var anchor Anchor
	started, startOK := hostedObservationTime(attempt.StartedAt)
	transitioned, transitionOK := hostedObservationTime(transitionedAt)
	if json.Unmarshal([]byte(anchorJSON), &anchor) != nil || !validEvidenceAnchor(anchor) ||
		anchor.RepositoryID != repositoryID || anchor.PullNumber != hold.PullNumber || anchor.CandidateSHA != attempt.HeadSHA ||
		!hostedObservationID(attempt.SourceAttemptID, "review_attempt_") || !startOK || !transitionOK || transitioned.Before(started) {
		return nil, nil
	}
	if attempt.TriggerID != "" || attempt.State == "active" || reason == "hosted_finished_inventory_unproven" {
		trigger, err := strconv.ParseInt(attempt.TriggerID, 10, 64)
		if err != nil || trigger <= 0 || strconv.FormatInt(trigger, 10) != attempt.TriggerID {
			return nil, nil
		}
	}
	if reviews.ValidAttemptReason(attempt.State, reason) {
		attempt.Reason = reason
	}
	return &hold, nil
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
