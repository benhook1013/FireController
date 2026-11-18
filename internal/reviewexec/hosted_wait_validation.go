package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ValidateOperationWaitsSnapshot checks retained scheduling and attempt ownership
// without provider calls or current freshness checks. A wait row may remain as
// immutable-history attribution after its current scheduling intent has ended.
func ValidateOperationWaitsSnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT coalesce(o.project_id,''),w.operation_id FROM review_operation_waits w LEFT JOIN review_operations o ON o.id=w.operation_id
		UNION SELECT project_id,id FROM review_operations WHERE reason=?
		UNION SELECT a.project_id,a.operation_id FROM review_execution_attempts a JOIN review_execution_transitions t ON t.attempt_id=a.id WHERE t.reason=?
		UNION SELECT a.project_id,a.operation_id FROM review_execution_attempts a
		LEFT JOIN review_operations o ON o.id=a.operation_id
		JOIN review_execution_transitions t ON t.attempt_id=a.id
		WHERE (o.id IS NULL OR o.attempt_id<>a.id) AND t.reason IN ('allowance_exhausted','hosted_allowance_cooldown')
		ORDER BY 2`, cliDraftWait, cliDraftWait)
	if err != nil {
		return err
	}
	defer rows.Close()
	// The owner scan and its validation use the same read transaction.
	// Keep each operation's facts only until that operation is checked.
	for rows.Next() {
		var project, operation string
		if err := rows.Scan(&project, &operation); err != nil {
			return err
		}
		o, err := readOperationBase(ctx, tx, project, operation)
		if err != nil {
			return err
		}
		if err := validateOperationWaitHistory(ctx, tx, o); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateOperationWaitHistory(ctx context.Context, tx *sql.Tx, o Operation) error {
	hasBaseline := o.Wait != nil && o.Wait.BaselineAttemptID != ""
	if hasBaseline && o.AttemptID == o.Wait.BaselineAttemptID {
		return waitUnavailable()
	}
	if o.Reason == cliDraftWait && (o.Channel != "cli" || o.State != "queued" || o.AttemptID != "") {
		return waitUnavailable()
	}
	if o.AttemptID == "" {
		if o.State != "queued" && o.State != "held" && o.State != "cancelled" {
			return waitUnavailable()
		}
	} else if o.State == "queued" || o.Wait != nil && o.Wait.Paused {
		return waitUnavailable()
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM review_execution_attempts WHERE operation_id=? ORDER BY id`, o.ID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var firstDeadline, lastDeadline, baselineDeadline time.Time
	foundCurrent := o.AttemptID == ""
	foundDraft := false
	for _, id := range ids {
		a, err := readAttempt(ctx, tx, o.ProjectID, id)
		if err != nil {
			return err
		}
		if id == o.AttemptID {
			if a.State != o.State || a.OperationID != o.ID || a.ProjectID != o.ProjectID {
				return waitUnavailable()
			}
			foundCurrent = true
			continue
		}
		if a.State == "cancelled" && hasBaseline {
			if err := validateUnqueriedRetryHistory(ctx, tx, o, a.ID, o.Wait.BaselineAttemptID); err != nil {
				return err
			}
			continue
		}
		_, draft, err := historicalCLIDraftOperation(ctx, tx, o, a)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return waitUnavailable()
			}
			return err
		}
		if draft {
			foundDraft = true
			continue
		}
		if !hasBaseline {
			return waitUnavailable()
		}
		if err := validateUnusedCooldownJournal(ctx, tx, a); err != nil {
			return err
		}
		historical, err := historicalCooldownOperation(ctx, tx, o, a)
		if err != nil {
			return err
		}
		var due time.Time
		if o.Channel == "cli" {
			record, err := readCLIAllowanceRecord(ctx, tx, historical, a)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) || storedAllowanceUnavailable(err) {
					return waitUnavailable()
				}
				return err
			}
			if record.Observation.Remaining != 0 {
				return waitUnavailable()
			}
			due, err = cliExhaustionDeadline(ctx, tx, a)
			if err != nil {
				return err
			}
		} else {
			record, err := readHostedAllowanceRecord(ctx, tx, historical, a)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) || storedAllowanceUnavailable(err) {
					return waitUnavailable()
				}
				return err
			}
			if record.Input.Observation.State != "cooldown_observed" {
				return waitUnavailable()
			}
			var at string
			if err := tx.QueryRowContext(ctx, `SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&at); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return waitUnavailable()
				}
				return err
			}
			ended, valid := attemptRecordedTime(at)
			if !valid || !record.Input.Observation.RetryNotBefore.After(ended) {
				return waitUnavailable()
			}
			due = record.Input.Observation.RetryNotBefore
		}
		if firstDeadline.IsZero() || due.Before(firstDeadline) {
			firstDeadline = due
		}
		if lastDeadline.IsZero() || due.After(lastDeadline) {
			lastDeadline = due
		}
		if id == o.Wait.BaselineAttemptID {
			baselineDeadline = due
		}
		var occupied bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=?)`, a.ID, a.ID).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return waitUnavailable()
		}
	}
	if !foundCurrent || o.Wait == nil && !foundDraft {
		return waitUnavailable()
	}
	if hasBaseline {
		if err := validateFirstCooldown(ctx, tx, o, o.Wait.BaselineAttemptID); err != nil {
			return err
		}
	}
	if hasBaseline && (baselineDeadline.IsZero() || !baselineDeadline.Equal(firstDeadline) || lastDeadline.UTC().Format(time.RFC3339Nano) != o.Wait.RetryNotBefore) {
		return waitUnavailable()
	}
	return nil
}

// Detached quota preparations have precisely the unused preparation and hold
// transitions; restore must not validate only their surviving last row.
func validateUnusedCooldownJournal(ctx context.Context, q EvidenceQuery, a Attempt) error {
	if a.Revision != 2 {
		return waitUnavailable()
	}
	rows, err := q.QueryContext(ctx, `SELECT revision,state,reason,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? ORDER BY revision`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := int64(1)
	var previous time.Time
	for rows.Next() {
		var rev int64
		var state, reason, external, stamp string
		if err := rows.Scan(&rev, &state, &reason, &external, &stamp); err != nil {
			return err
		}
		at, ok := attemptRecordedTime(stamp)
		if rev != expected || expected > 2 || external != "" || !ok || at.Before(previous) {
			return waitUnavailable()
		}
		if expected == 1 && (state != "preparing" || reason != "preparing") {
			return waitUnavailable()
		}
		if expected == 2 && (state != "held" || (reason != "hosted_allowance_cooldown" && reason != "allowance_exhausted")) {
			return waitUnavailable()
		}
		previous = at
		expected++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if expected != 3 {
		return waitUnavailable()
	}
	return nil
}
