package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// CLI quota waits retain explicit requests, not a promise about provider reset.
// The account-wide backoff avoids probing once per queued PR or project.
func cliRetryDelay(previous int64) time.Duration {
	if previous > 3 {
		previous = 3
	}
	return min(time.Minute*time.Duration(1<<previous), 5*time.Minute)
}

func historicalCLIExhaustionOperation(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (Operation, error) {
	if o.Channel != "cli" || a.OperationID != o.ID || a.ProjectID != o.ProjectID || a.State != "held" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" {
		return Operation{}, waitUnavailable()
	}
	var state, reason string
	if err := q.QueryRowContext(ctx, `SELECT state,reason FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&state, &reason); err != nil || state != "held" || reason != "allowance_exhausted" {
		return Operation{}, waitUnavailable()
	}
	var owns bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operation_waits WHERE operation_id=? AND baseline_attempt_id IS NOT NULL)`, o.ID).Scan(&owns); err != nil || !owns {
		return Operation{}, waitUnavailable()
	}
	o.AttemptID, o.State, o.Reason, o.Wait = a.ID, "held", reason, nil
	return o, nil
}

// The deadline derives solely from the retained unused transition, never from
// FullCapacityInMS (which does not identify the next available review slot).
func cliExhaustionDeadline(ctx context.Context, q EvidenceQuery, a Attempt) (time.Time, error) {
	var at string
	if err := q.QueryRowContext(ctx, `SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=? AND state='held' AND reason='allowance_exhausted'`, a.ID, a.Revision).Scan(&at); err != nil {
		return time.Time{}, waitUnavailable()
	}
	ended, valid := attemptRecordedTime(at)
	if !valid {
		return time.Time{}, waitUnavailable()
	}
	var previous int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM review_execution_transitions t JOIN review_execution_attempts a ON a.id=t.attempt_id WHERE a.operation_id=? AND t.state='held' AND t.reason='allowance_exhausted' AND julianday(t.created_at)<julianday(?)`, a.OperationID, at).Scan(&previous); err != nil {
		return time.Time{}, err
	}
	return ended.Add(cliRetryDelay(previous)), nil
}

func (e *Engine) ScheduleCLICooldown(ctx context.Context, project, attemptID string, revision int64) (Attempt, error) {
	var result Attempt
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		result, err = e.scheduleCLICooldown(ctx, tx, project, attemptID, revision)
		return err
	})
	if err == nil {
		e.notifyOperation(result.OperationID)
	}
	return result, err
}

func (e *Engine) scheduleCLICooldown(ctx context.Context, tx *sql.Tx, project, attemptID string, revision int64) (Attempt, error) {
	a, err := readAttempt(ctx, tx, project, attemptID)
	if err != nil {
		return Attempt{}, err
	}
	o, err := readOperationBase(ctx, tx, project, a.OperationID)
	if err != nil {
		return Attempt{}, err
	}
	if a.Revision != revision || a.State != "preparing" || o.State != "preparing" || o.AttemptID != a.ID || o.Channel != "cli" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || a.Revision == math.MaxInt64 || o.Revision == math.MaxInt64 {
		return Attempt{}, conflict()
	}
	record, err := readCLIAllowanceRecord(ctx, tx, o, a)
	if err != nil {
		return Attempt{}, err
	}
	if record.Observation.Remaining != 0 {
		return Attempt{}, waitUnavailable()
	}
	at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleExecutionTime())
	if err != nil {
		return Attempt{}, err
	}
	baseline := a.ID
	if o.Wait != nil && o.Wait.BaselineAttemptID != "" {
		baseline = o.Wait.BaselineAttemptID
	}
	a.State, a.Revision = "held", a.Revision+1
	if _, err := tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state='held',revision=? WHERE id=?`, a.Revision, a.ID); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='held',reason='allowance_exhausted',revision=revision+1,updated_at=? WHERE id=?`, at, o.ID); err != nil {
		return Attempt{}, err
	}
	if err := e.transitionRow(ctx, tx, a, "allowance_exhausted", at); err != nil {
		return Attempt{}, err
	}
	due, err := cliExhaustionDeadline(ctx, tx, a)
	if err != nil {
		return Attempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,?,?,0) ON CONFLICT(operation_id) DO UPDATE SET baseline_attempt_id=excluded.baseline_attempt_id,retry_not_before=excluded.retry_not_before`, o.ID, baseline, due.UTC().Format(time.RFC3339Nano)); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='queued',reason='',attempt_id='' WHERE id=?`, o.ID); err != nil {
		return Attempt{}, err
	}
	if err := core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
		return Attempt{}, err
	}
	return a, nil
}

func cliAccountRetryDeadline(ctx context.Context, q EvidenceQuery, capability Capability, now time.Time) (time.Time, error) {
	var project, id string
	// Retained deadlines are canonical UTC RFC3339Nano. Removing the final Z
	// orders whole and fractional seconds exactly, unlike julianday's rounding.
	// Expired audit history cannot impose a live account-wide scheduling hold.
	err := q.QueryRowContext(ctx, `SELECT o.project_id,o.id FROM review_operation_waits w
	JOIN review_operations o ON o.id=w.operation_id
	JOIN review_execution_attempts a ON a.id=w.baseline_attempt_id
	WHERE o.channel='cli' AND a.provider=? AND a.account=? AND w.retry_not_before<>''
	AND rtrim(w.retry_not_before,'Z')>rtrim(?,'Z')
	ORDER BY rtrim(w.retry_not_before,'Z') DESC LIMIT 1`, capability.Provider, capability.Account, now.UTC().Format(time.RFC3339Nano)).Scan(&project, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	o, err := readOperationBase(ctx, q, project, id)
	if err != nil {
		return time.Time{}, err
	}
	if o.Wait == nil || o.Wait.RetryNotBefore == "" {
		return time.Time{}, waitUnavailable()
	}
	return mustWaitTime(o.Wait.RetryNotBefore), nil
}
