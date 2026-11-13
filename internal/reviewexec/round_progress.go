package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
)

// RoundProgressChannel contains current native allowance facts and optional
// private activity diagnostics. Pending counts operations bound to this epoch,
// not coverage or permission to dispatch.
type RoundProgressChannel struct {
	ActivityEvidence    *ActivityEvidence `json:"activity_evidence,omitempty"`
	CancelledWait       *CancelledWait    `json:"cancelled_wait,omitempty"`
	Wait                *OperationWait    `json:"wait,omitempty"`
	SourceRetry         *SourceRetry      `json:"source_retry,omitempty"`
	Channel             string            `json:"channel"`
	Configured          bool              `json:"configured"`
	Revision            int64             `json:"revision"`
	Epoch               int64             `json:"epoch"`
	Closed              bool              `json:"closed"`
	Minimum             int64             `json:"minimum"`
	Maximum             int64             `json:"maximum"`
	MaximumUnbounded    bool              `json:"maximum_unbounded,omitempty"`
	Taper               int64             `json:"taper"`
	Used                int64             `json:"used"`
	Dry                 int64             `json:"dry"`
	Tapered             bool              `json:"tapered"`
	Pending             int64             `json:"pending"`
	PendingExecution    *int64            `json:"pending_execution,omitempty"`
	PendingAdjudication *int64            `json:"pending_adjudication,omitempty"`
}

// CancelledWait identifies the unused baseline of an explicitly cancelled queued request.
type CancelledWait struct {
	BaselineAttemptID string `json:"baseline_attempt_id"`
	HeadSHA           string `json:"head_sha"`
	CancelledAt       string `json:"cancelled_at"`
}

type RoundProgressItem struct {
	PullNumber int64                  `json:"pull_number"`
	Channels   []RoundProgressChannel `json:"channels"`
}

type RoundProgress struct {
	ProjectID   string              `json:"project_id"`
	EvaluatedAt string              `json:"evaluated_at"`
	Items       []RoundProgressItem `json:"items"`
}

func validRoundProgressNumbers(numbers []int64) bool {
	if len(numbers) < 1 || len(numbers) > 100 {
		return false
	}
	seen := make(map[int64]bool, len(numbers))
	for _, number := range numbers {
		if number < 1 || number > 2147483647 || seen[number] {
			return false
		}
		seen[number] = true
	}
	return true
}

// ReadRoundProgress reads one bounded, authorized local snapshot. It does not
// interpret imported history or collect source, captures, CI or provider state.
func (e *Engine) ReadRoundProgress(ctx context.Context, scope core.Scope, numbers []int64) (RoundProgress, error) {
	return e.readRoundProgress(ctx, scope, numbers, false)
}

// ReadRoundProgressStages splits the same occupancy into execution and
// adjudication without changing reservation or completion accounting.
func (e *Engine) ReadRoundProgressStages(ctx context.Context, scope core.Scope, numbers []int64) (RoundProgress, error) {
	return e.readRoundProgress(ctx, scope, numbers, true)
}

func (e *Engine) readRoundProgress(ctx context.Context, scope core.Scope, numbers []int64, stages bool, activity ...bool) (RoundProgress, error) {
	return e.readRoundProgressWithQueuePriority(ctx, scope, numbers, stages, false, activity...)
}
func (e *Engine) readRoundProgressWithQueuePriority(ctx context.Context, scope core.Scope, numbers []int64, stages, queuePriority bool, activity ...bool) (RoundProgress, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(scope.ProjectID) || !validRoundProgressNumbers(numbers) {
		return RoundProgress{}, invalid()
	}
	numbers = append([]int64(nil), numbers...)
	out := RoundProgress{ProjectID: scope.ProjectID, Items: make([]RoundProgressItem, 0, len(numbers))}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		for _, number := range numbers {
			item := RoundProgressItem{PullNumber: number, Channels: make([]RoundProgressChannel, 0, 2)}
			for _, channel := range []string{"hosted", "cli"} {
				control, err := readRoundRecord(ctx, tx, scope.ProjectID, number, channel)
				if err != nil {
					return err
				}
				var execution, adjudication int64
				if control.Configured {
					execution, adjudication, err = roundOccupancyStages(ctx, tx, control, "")
					if err != nil {
						return err
					}
				}
				progress := RoundProgressChannel{
					Channel: channel, Configured: control.Configured, Revision: control.Revision,
					Epoch: control.Epoch, Closed: control.Closed, Minimum: control.Minimum,
					Maximum: control.Maximum, MaximumUnbounded: control.MaximumUnbounded, Taper: control.Taper, Used: control.Used,
					Dry: control.Dry, Tapered: control.Tapered, Pending: execution + adjudication,
				}
				if stages {
					progress.PendingExecution, progress.PendingAdjudication = &execution, &adjudication
					progress.Wait, err = e.readChannelWait(ctx, tx, scope.ProjectID, number, channel, queuePriority)
					if err != nil {
						return err
					}
					if progress.Wait == nil && progress.Pending == 0 {
						progress.CancelledWait, err = readChannelCancelledWait(ctx, tx, scope.ProjectID, number, channel)
						if err != nil {
							return err
						}
					}
				}
				if len(activity) > 0 && activity[0] {
					progress.ActivityEvidence, err = readActivityEvidence(ctx, tx, scope.ProjectID, number, channel)
					if err != nil {
						return err
					}
				}
				if len(activity) > 1 && activity[1] {
					progress.SourceRetry, err = readChannelSourceRetry(ctx, tx, scope.ProjectID, number, channel)
					if err != nil {
						return err
					}
				}
				item.Channels = append(item.Channels, progress)
			}
			out.Items = append(out.Items, item)
		}
		out.EvaluatedAt = stamp()
		return nil
	})
	if err != nil {
		return RoundProgress{}, err
	}
	return out, nil
}

// Source-only waits need not have an allowance timer. Only read the retry
// attached to a current queued operation, never an older stopped attempt.
func readChannelSourceRetry(ctx context.Context, tx *sql.Tx, project string, number int64, channel string) (*SourceRetry, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM review_operations WHERE project_id=? AND pull_number=? AND channel=? AND state='queued' AND reason=? AND attempt_id='' ORDER BY created_at,id LIMIT 1`, project, number, channel, sourceRetryReason).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := readOperationBase(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	if o.Wait == nil || o.Wait.SourceRetry == nil {
		return nil, waitUnavailable()
	}
	return o.Wait.SourceRetry, nil
}

// Project only a current queued intent, never infer a timer from a held run.
func (e *Engine) readChannelWait(ctx context.Context, tx *sql.Tx, project string, number int64, channel string, queuePriority bool) (*OperationWait, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT o.id FROM review_operations o JOIN review_operation_waits w ON w.operation_id=o.id WHERE o.project_id=? AND o.pull_number=? AND o.channel=? AND o.state='queued' AND o.attempt_id='' ORDER BY o.created_at,o.id LIMIT 1`, project, number, channel).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := readOperationBase(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	wait := currentOperationWait(o)
	if !queuePriority || channel != "hosted" || wait == nil || wait.Paused {
		return wait, nil
	}
	// Reuse actual accepted-intent admission priority, not the first PR with
	// remaining rounds. The latter may be skipped legitimately by lookahead.
	// These are bounded local reads in this same authorized snapshot; no source
	// refresh or provider request is performed. An unavailable hint leaves the
	// original retained timer intact rather than fabricating a queue blocker.
	configuration, revision, err := readConfiguration(ctx, tx, project)
	if err != nil {
		return wait, nil
	}
	earlier, err := e.earlierQueuedReviewIntent(ctx, tx, o, configuration, revision)
	if err == nil && earlier != nil {
		copy := *wait
		copy.QueueAheadPullNumber = earlier.PullNumber
		wait = &copy
	}
	return wait, nil
}

func readChannelCancelledWait(ctx context.Context, tx *sql.Tx, project string, number int64, channel string) (*CancelledWait, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT o.id FROM review_operations o JOIN review_operation_waits w ON w.operation_id=o.id WHERE o.project_id=? AND o.pull_number=? AND o.channel=? AND o.state='cancelled' AND o.attempt_id='' AND w.baseline_attempt_id IS NOT NULL ORDER BY o.created_at DESC,o.id DESC LIMIT 1`, project, number, channel).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// This validates the original unused attempt, its owning operation and the
	// retained allowance proof, not just an arbitrary held/cancelled state pair.
	o, err := readOperationBase(ctx, tx, project, id)
	if err != nil {
		return nil, err
	}
	if o.ProjectID != project || o.PullNumber != number || o.Channel != channel || o.State != "cancelled" || o.AttemptID != "" || o.Wait == nil || o.Wait.BaselineAttemptID == "" || !sha.MatchString(o.HeadSHA) {
		return nil, waitUnavailable()
	}
	if _, ok := attemptRecordedTime(o.UpdatedAt); !ok {
		return nil, waitUnavailable()
	}
	return &CancelledWait{BaselineAttemptID: o.Wait.BaselineAttemptID, HeadSHA: o.HeadSHA, CancelledAt: o.UpdatedAt}, nil
}
