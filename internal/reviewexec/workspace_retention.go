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
	var stamp string
	err := e.store.DB().QueryRowContext(ctx, `SELECT t.created_at
 FROM review_execution_attempts a
 JOIN review_operations o ON o.id=a.operation_id AND o.attempt_id=a.id AND o.project_id=a.project_id AND o.state=a.state
 JOIN review_execution_config c ON c.project_id=o.project_id AND c.revision=o.configuration_revision
 JOIN review_execution_transitions t ON t.attempt_id=a.id AND t.revision=a.revision AND t.state=a.state
 WHERE a.id=? AND a.provider=? AND a.account=? AND o.channel IN ('cli','hosted')
 AND a.state IN ('completed','held','failed','cancelled','rate_limited')
 AND NOT EXISTS(SELECT 1 FROM review_execution_reservations r WHERE r.attempt_id=a.id)
 AND json_valid(c.body_json) AND json_extract(c.body_json,'$.capability_id')=?`, attemptID, capability.Provider, capability.Account, capabilityID).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return err == nil && !at.After(now.Add(-5*time.Minute)), nil
}
