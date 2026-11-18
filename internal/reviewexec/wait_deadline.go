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
				AND julianday(json_extract(p.body_json,'$.input.observation.RetryNotBefore')) IS NOT NULL
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
		SELECT a.id,` + deadline + ` FROM latest a LEFT JOIN review_hosted_preflight p ON p.attempt_id=a.id ORDER BY a.id DESC`
	}
	var id string
	var err error
	if o.Channel == "hosted" {
		rows, queryErr := q.QueryContext(ctx, query, o.ID, o.AttemptID, reason)
		if queryErr != nil {
			return Attempt{}, queryErr
		}
		var greatest time.Time
		invalidSelected := false
		for rows.Next() {
			var candidate string
			var raw sql.NullString
			if err = rows.Scan(&candidate, &raw); err != nil {
				rows.Close()
				return Attempt{}, err
			}
			due, valid := hostedObservationTime(raw.String)
			if !raw.Valid || !valid {
				if !invalidSelected {
					id, invalidSelected = candidate, true
				}
			} else if !invalidSelected && (id == "" || due.After(greatest)) {
				id, greatest = candidate, due
			}
		}
		err = rows.Err()
		rows.Close()
		if err == nil && id == "" {
			err = sql.ErrNoRows
		}
	} else {
		err = q.QueryRowContext(ctx, query, o.ID, o.AttemptID, reason).Scan(&id)
	}
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

// First-cooldown identity comes from the retained preparation chronology, never
// a random ID or provider deadline. Equal first timestamps cannot establish an
// immutable first comparison and are refused rather than guessed.
func validateFirstCooldown(ctx context.Context, q EvidenceQuery, o Operation, baseline string) error {
	reason := "hosted_allowance_cooldown"
	if o.Channel == "cli" {
		reason = "allowance_exhausted"
	}
	rows, err := q.QueryContext(ctx, `SELECT a.id,p.created_at,t.state,t.reason,a.state FROM review_execution_attempts a
 LEFT JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision
 LEFT JOIN review_execution_transitions p ON p.attempt_id=a.id AND p.revision=1 AND p.state='preparing'
 WHERE a.operation_id=? AND a.id<>?`, o.ID, o.AttemptID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var cancelled []string
	var first string
	var earliest time.Time
	tied := false
	for rows.Next() {
		var id string
		var stamp, state, retainedReason sql.NullString
		var attemptState string
		if err := rows.Scan(&id, &stamp, &state, &retainedReason, &attemptState); err != nil {
			return err
		}
		if attemptState == "cancelled" {
			cancelled = append(cancelled, id)
			continue
		}
		if !state.Valid || !retainedReason.Valid || state.String != "held" || attemptState != "held" {
			return waitUnavailable()
		}
		if o.Channel == "cli" && retainedReason.String == cliDraftWait {
			continue
		}
		if retainedReason.String != reason {
			return waitUnavailable()
		}
		at, ok := attemptRecordedTime(stamp.String)
		if !stamp.Valid || !ok {
			return waitUnavailable()
		}
		if first == "" || at.Before(earliest) {
			first, earliest, tied = id, at, false
		} else if at.Equal(earliest) {
			tied = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, id := range cancelled {
		if err := validateUnqueriedRetryHistory(ctx, q, o, id, baseline); err != nil {
			return err
		}
	}
	if tied || first == "" || first != baseline {
		return waitUnavailable()
	}
	return nil
}
