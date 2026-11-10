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
		UNION SELECT o.project_id,o.id FROM review_operations o JOIN review_execution_attempts a ON a.operation_id=o.id JOIN review_execution_transitions t ON t.attempt_id=a.id WHERE a.id<>o.attempt_id AND t.reason IN ('pull_scheduling_paused','automation_authorization_changed')
		ORDER BY 2`, cliDraftWait, cliDraftWait)
	if err != nil {
		return err
	}
	var owners [][2]string
	for rows.Next() {
		var owner [2]string
		if err := rows.Scan(&owner[0], &owner[1]); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, owner)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, owner := range owners {
		o, err := readOperationBase(ctx, tx, owner[0], owner[1])
		if err != nil {
			return err
		}
		if err := validateOperationWaitHistory(ctx, tx, o); err != nil {
			return err
		}
	}
	return nil
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
	foundDraft := o.Reason == cliDraftWait
	foundSource := false
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
		_, draft, err := historicalUnusedPreparationOperation(ctx, tx, o, a, true)
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
		_, source, err := historicalSourceRetryOperation(ctx, tx, o, a)
		if err != nil {
			return err
		}
		if source {
			foundSource = true
			continue
		}
		if !hasBaseline {
			return waitUnavailable()
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
	if !foundCurrent || o.Wait == nil && !foundDraft && !foundSource {
		return waitUnavailable()
	}
	if hasBaseline && (baselineDeadline.IsZero() || !baselineDeadline.Equal(firstDeadline) || lastDeadline.UTC().Format(time.RFC3339Nano) != o.Wait.RetryNotBefore) {
		return waitUnavailable()
	}
	return nil
}
