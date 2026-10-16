package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func attemptProjectionCapacity(ctx context.Context, tx *sql.Tx, operation Operation) (bool, error) {
	_, err := currentReviewPull(ctx, tx, operation.ProjectID, operation.PullNumber)
	if err != nil && !missingCurrentPull(err) {
		return false, err
	}
	return reviews.NativeAttemptCapacity(ctx, tx, operation.ProjectID, operation.PullNumber)
}

// Only the named absent-row result permits automatic ledger-only operation.
func missingCurrentPull(err error) bool {
	var failure *core.Error
	return errors.As(err, &failure) && failure.Code == "review_projection_missing"
}

func publishAttemptTransition(ctx context.Context, tx *sql.Tx, attempt Attempt) error {
	_, err := publishJournalAttempt(ctx, tx, attempt, nil, true)
	return err
}

// PublishAttempt explicitly projects a retained attempt. A nil metadata argument
// requires existing current PR facts. Supplied metadata is an initial fallback
// only: the same publication transaction prefers any existing current PR row.
// Captured source fields are never used as current PR metadata.
func (e *Engine) PublishAttempt(ctx context.Context, project, attemptID string, metadata *reviews.PullInput) (reviews.Receipt, error) {
	if !identifier.MatchString(project) || !identifier.MatchString(attemptID) {
		return reviews.Receipt{}, invalid()
	}
	var supplied *reviews.PullInput
	if metadata != nil {
		copy := *metadata
		if len(copy.Runs) != 0 || len(copy.Attempts) != 0 {
			return reviews.Receipt{}, invalid()
		}
		supplied = &copy
	}
	var receipt reviews.Receipt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		attempt, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		receipt, err = publishJournalAttempt(ctx, tx, attempt, supplied, false)
		return err
	})
	if err != nil {
		return reviews.Receipt{}, err
	}
	return receipt, nil
}

func publishJournalAttempt(ctx context.Context, tx *sql.Tx, attempt Attempt, metadata *reviews.PullInput, allowMissing bool) (reviews.Receipt, error) {
	operation, err := readOperation(ctx, tx, attempt.ProjectID, attempt.OperationID)
	if err != nil {
		return reviews.Receipt{}, err
	}
	if operation.AttemptID != attempt.ID || operation.State != attempt.State || !validChannel(operation.Channel) || !sha.MatchString(operation.HeadSHA) {
		return reviews.Receipt{}, publicationConflict()
	}
	if metadata != nil && metadata.Number != operation.PullNumber {
		return reviews.Receipt{}, publicationConflict()
	}
	pull, err := currentReviewPull(ctx, tx, attempt.ProjectID, operation.PullNumber)
	if missingCurrentPull(err) && metadata != nil {
		pull, err = *metadata, nil
	} else if missingCurrentPull(err) && allowMissing {
		return reviews.Receipt{}, nil
	}
	if err != nil {
		return reviews.Receipt{}, err
	}
	if pull.Number != operation.PullNumber {
		return reviews.Receipt{}, publicationConflict()
	}
	projected, err := journalBrowserAttempt(ctx, tx, operation, attempt)
	if err != nil {
		return reviews.Receipt{}, err
	}
	receipt, unchanged, err := attemptProjectionReplay(ctx, tx, attempt.ProjectID, operation.PullNumber, projected)
	if err != nil || unchanged {
		return receipt, err
	}
	if allowMissing {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_workspace w JOIN review_attempts a ON a.project_id=w.project_id AND a.snapshot_id=w.snapshot_id WHERE w.project_id=? AND a.source='native-executor' AND a.source_attempt_id=?)`, attempt.ProjectID, attempt.ID).Scan(&exists); err != nil {
			return reviews.Receipt{}, err
		}
		if !exists {
			available, err := reviews.NativeAttemptCapacity(ctx, tx, attempt.ProjectID, operation.PullNumber)
			if err != nil || !available {
				// A replacement snapshot may omit this live attempt and fill the
				// projection. Keep automatic transitions ledger-only without
				// copying any snapshot rows; explicit publication still refuses.
				return reviews.Receipt{}, err
			}
		}
	}
	return reviews.PublishNative(ctx, tx, core.Scope{ProjectID: operation.ProjectID, PrincipalID: operation.ActorID, WorkerID: operation.WorkerID}, reviews.NativePublication{Pull: pull, Attempt: &projected})
}

func journalBrowserAttempt(ctx context.Context, tx *sql.Tx, operation Operation, attempt Attempt) (reviews.Attempt, error) {
	projected := reviews.Attempt{SourceAttemptID: attempt.ID, Channel: operation.Channel, HeadSHA: operation.HeadSHA, State: attempt.State}
	var firstRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision,created_at FROM review_execution_transitions WHERE attempt_id=? ORDER BY revision LIMIT 1`, attempt.ID).Scan(&firstRevision, &projected.StartedAt); err != nil {
		return projected, err
	}
	var currentAt, currentState string
	if err := tx.QueryRowContext(ctx, `SELECT state,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, attempt.ID, attempt.Revision).Scan(&currentState, &currentAt); err != nil {
		return projected, err
	}
	started, startErr := time.Parse(time.RFC3339Nano, projected.StartedAt)
	finished, finishErr := time.Parse(time.RFC3339Nano, currentAt)
	if firstRevision != 1 || currentState != attempt.State || startErr != nil || finishErr != nil || started.IsZero() || finished.IsZero() || finished.Before(started) {
		return projected, core.Fail("storage_unavailable", "Stored review attempt timing is unavailable.", 500)
	}
	switch attempt.State {
	case "completed", "failed", "cancelled", "rate_limited", "held":
		projected.FinishedAt = currentAt
		seconds := finished.Unix() - started.Unix()
		if finished.Nanosecond() < started.Nanosecond() {
			seconds--
		}
		projected.DurationSeconds = &seconds
	case "preparing", "posting", "active", "uncertain":
	default:
		return projected, publicationConflict()
	}
	if operation.Channel == "hosted" {
		projected.TriggerID = attempt.ExternalID
	}
	// A capture reference is visible only after its actual native run is part of
	// the current snapshot. Capture retention by itself is not publication.
	err := tx.QueryRowContext(ctx, `SELECT e.id FROM review_execution_evidence e
 JOIN review_workspace w ON w.project_id=e.project_id
 JOIN review_runs r ON r.project_id=w.project_id AND r.snapshot_id=w.snapshot_id AND r.source='native-executor' AND r.source_run_id=e.id
 WHERE e.project_id=? AND e.attempt_id=? AND e.operation_id=? AND r.pull_number=? AND r.channel=? AND r.head_sha=?`, attempt.ProjectID, attempt.ID, operation.ID, operation.PullNumber, operation.Channel, operation.HeadSHA).Scan(&projected.SourceRunID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return projected, err
	}
	return projected, nil
}

func attemptProjectionReplay(ctx context.Context, tx *sql.Tx, project string, number int64, expected reviews.Attempt) (reviews.Receipt, bool, error) {
	var receipt reviews.Receipt
	var actual reviews.Attempt
	var actualNumber int64
	err := tx.QueryRowContext(ctx, `SELECT w.snapshot_id,w.revision,a.pull_number,a.source_attempt_id,a.channel,a.head_sha,a.state,a.started_at,a.finished_at,a.duration_seconds,a.source_run_id,a.trigger_id,a.provider_review_id
 FROM review_workspace w JOIN review_attempts a ON a.project_id=w.project_id AND a.snapshot_id=w.snapshot_id
 WHERE w.project_id=? AND a.source='native-executor' AND a.source_attempt_id=?`, project, expected.SourceAttemptID).Scan(&receipt.ID, &receipt.Revision, &actualNumber, &actual.SourceAttemptID, &actual.Channel, &actual.HeadSHA, &actual.State, &actual.StartedAt, &actual.FinishedAt, &actual.DurationSeconds, &actual.SourceRunID, &actual.TriggerID, &actual.ProviderReviewID)
	if errors.Is(err, sql.ErrNoRows) {
		return reviews.Receipt{}, false, nil
	}
	if err != nil {
		return reviews.Receipt{}, false, err
	}
	if actualNumber != number {
		return reviews.Receipt{}, false, publicationConflict()
	}
	if encoded(actual) != encoded(expected) {
		return reviews.Receipt{}, false, nil
	}
	receipt.Outcome = "observed"
	return receipt, true, nil
}
