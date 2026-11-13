package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// WorkspaceDisposable is an internal runtime read, not an agent permission.
// Terminal attempts cannot be restarted. Their immutable captures/evidence and
// journal remain retained separately; an account reservation always wins.
func (e *Engine) WorkspaceDisposable(ctx context.Context, capabilityID, attemptID string, now time.Time) (bool, error) {
	if e == nil || ctx == nil || !identifier.MatchString(attemptID) {
		return false, invalid()
	}
	capability, ok := e.capabilities[capabilityID]
	if !ok {
		return false, conflict()
	}
	tx, err := e.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var stamp, created, updated string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT t.created_at,o.created_at,o.updated_at,a.revision
 FROM review_execution_attempts a
 JOIN review_operations o ON o.id=a.operation_id AND o.project_id=a.project_id
 JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
 JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision AND t.state=a.state
 WHERE a.id=? AND a.provider=? AND a.account=? AND o.channel IN ('cli','hosted')
 AND a.state IN ('completed','held','failed','cancelled','rate_limited')
 AND ((o.attempt_id=a.id AND o.state=a.state)
 OR (((o.channel='hosted' AND t.reason='hosted_allowance_cooldown') OR (o.channel='cli' AND t.reason='allowance_exhausted')) AND a.state='held' AND a.anchor_json='null' AND a.intent='' AND a.external_id=''
 AND EXISTS(SELECT 1 FROM review_operation_waits w WHERE w.operation_id=o.id AND w.baseline_attempt_id IS NOT NULL)))
 AND NOT EXISTS(SELECT 1 FROM review_execution_reservations r WHERE r.attempt_id=a.id)
 AND json_valid(c.body_json) AND json_extract(c.body_json,'$.capability_id')=?`, attemptID, capability.Provider, capability.Account, capabilityID).Scan(&stamp, &created, &updated, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at, valid := attemptRecordedTime(stamp)
	start, startOK := attemptRecordedTime(created)
	last, lastOK := attemptRecordedTime(updated)
	if !valid || !startOK || !lastOK || last.Before(start) || at.Before(start) || at.After(last) {
		return false, persistenceFailure()
	}
	rows, err := tx.QueryContext(ctx, `SELECT revision,created_at FROM review_execution_transitions WHERE attempt_id=? ORDER BY revision`, attemptID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	previous, expected := start, int64(1)
	for rows.Next() {
		var rev int64
		var raw string
		if err := rows.Scan(&rev, &raw); err != nil {
			return false, err
		}
		current, ok := attemptRecordedTime(raw)
		if !ok || rev != expected || current.Before(previous) || current.After(at) {
			return false, persistenceFailure()
		}
		previous, expected = current, expected+1
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if expected != revision+1 || !previous.Equal(at) {
		return false, persistenceFailure()
	}
	return !at.After(now.Add(-5 * time.Minute)), nil
}
