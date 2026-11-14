package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The baseline pins the first comparison, not the latest scheduling deadline.
// Select one detached cooldown in the caller's snapshot; other unused holds and
// a current preparation do not replace the retained timer's source.
func latestCooldownAttempt(ctx context.Context, q EvidenceQuery, o Operation) (Attempt, error) {
	reason := "hosted_allowance_cooldown"
	query := `SELECT a.id FROM review_execution_attempts a
	JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision
	WHERE a.operation_id=? AND a.id<>? AND t.state='held' AND t.reason=?
	ORDER BY rtrim(t.created_at,'Z') DESC,a.id DESC LIMIT 1`
	if o.Channel == "cli" {
		reason = "allowance_exhausted"
	} else {
		// Recording timestamps may tie after clock rollback. Hosted retries
		// advance the provider deadline even then; random attempt IDs cannot
		// establish their order. Read only a bounded scalar for tie selection,
		// and validate the winning record below. Unreadable tied proof wins
		// selection so it is refused rather than hidden by an older deadline.
		deadline := `CASE WHEN typeof(p.body_json)='text' AND length(CAST(p.body_json AS BLOB))<=16384 THEN
			CASE WHEN json_valid(p.body_json) THEN
				CASE WHEN json_type(p.body_json,'$.input.observation.RetryNotBefore')='text'
				THEN NULLIF(json_extract(p.body_json,'$.input.observation.RetryNotBefore'),'') END
			END END`
		// Narrow by recorded transition time before touching preflight bodies.
		// MATERIALIZED prevents SQLite from flattening the tie set back into
		// a history-wide JSON sort. All equal-time records still participate.
		query = `WITH cooldowns AS MATERIALIZED (
			SELECT a.id,rtrim(t.created_at,'Z') AS recorded_at
			FROM review_execution_attempts a
			JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision
			WHERE a.operation_id=? AND a.id<>? AND t.state='held' AND t.reason=?
		), latest AS MATERIALIZED (
			SELECT id FROM cooldowns WHERE recorded_at=(SELECT MAX(recorded_at) FROM cooldowns)
		)
		SELECT a.id FROM latest a LEFT JOIN review_hosted_preflight p ON p.attempt_id=a.id
		ORDER BY (` + deadline + ") IS NULL DESC,rtrim(" + deadline + ",'Z') DESC,a.id DESC LIMIT 1"
	}
	var id string
	err := q.QueryRowContext(ctx, query, o.ID, o.AttemptID, reason).Scan(&id)
	if err != nil {
		return Attempt{}, waitDependencyError(err)
	}
	a, err := readAttempt(ctx, q, o.ProjectID, id)
	return a, waitDependencyError(err)
}

func retainedCooldownDeadline(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (time.Time, error) {
	original, err := historicalCooldownOperation(ctx, q, o, a)
	if err != nil {
		return time.Time{}, waitDependencyError(err)
	}
	if o.Channel == "cli" {
		record, err := readCLIAllowanceRecord(ctx, q, original, a)
		if err != nil {
			return time.Time{}, waitDependencyError(err)
		}
		if record.Observation.Remaining != 0 {
			return time.Time{}, waitUnavailable()
		}
		return cliExhaustionDeadline(ctx, q, a)
	}
	record, err := readHostedAllowanceRecord(ctx, q, original, a)
	if err != nil {
		return time.Time{}, waitDependencyError(err)
	}
	if record.Input.Observation.State != "cooldown_observed" {
		return time.Time{}, waitUnavailable()
	}
	var at string
	if err := q.QueryRowContext(ctx, `SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, waitUnavailable()
		}
		return time.Time{}, err
	}
	ended, valid := attemptRecordedTime(at)
	if !valid || !record.Input.Observation.RetryNotBefore.After(ended) {
		return time.Time{}, waitUnavailable()
	}
	return record.Input.Observation.RetryNotBefore, nil
}
