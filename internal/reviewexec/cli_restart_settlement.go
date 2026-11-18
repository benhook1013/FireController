package reviewexec

import (
	"context"
	"database/sql"
	"slices"
)

// CLIRestartSettlementReason marks a CLI result that a daemon restart cut off.
const CLIRestartSettlementReason = "cli_restart_settled"

// RestartedCLIAttempts lists uncertain CLI attempts interrupted by a restart
// whose process is proven ended and whose result is not complete. The runtime
// settles each one once no unrecorded raw capture remains for it. Callers
// rotate after so skipped attempts cannot starve later ones.
func (e *Engine) RestartedCLIAttempts(ctx context.Context, capabilityID, after string) ([]RestartedCLIAttempt, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(capabilityID) {
		return nil, invalid()
	}
	capability, ok := e.capabilities[capabilityID]
	if !ok || !slices.Contains(capability.Channels, "cli") {
		return nil, conflict()
	}
	if !e.restartEndsCLI {
		return nil, nil
	}
	var result []RestartedCLIAttempt
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT a.project_id,a.id,a.revision,v.id IS NOT NULL
FROM review_operations o
JOIN review_execution_attempts a ON a.id=o.attempt_id AND a.operation_id=o.id AND a.project_id=o.project_id AND a.state=o.state
JOIN review_execution_reservations r ON r.attempt_id=a.id AND r.action_class='cli'
LEFT JOIN review_execution_evidence v ON v.attempt_id=a.id
WHERE o.channel='cli' AND o.state='uncertain' AND o.reason='supervisor_interrupted' AND r.execution_ended_at<>''
AND a.provider=? AND a.account=? AND o.capability_fingerprint=?
AND (v.id IS NULL OR NOT (json_valid(v.body_json) AND json_extract(v.body_json,'$.capture.review.Complete')=1 AND json_extract(v.body_json,'$.capture.review.Outcome')='completed'))
AND a.id>? ORDER BY a.id LIMIT 32`, capability.Provider, capability.Account, capability.Fingerprint, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x RestartedCLIAttempt
			if err := rows.Scan(&x.ProjectID, &x.AttemptID, &x.Revision, &x.HasEvidence); err != nil {
				return err
			}
			result = append(result, x)
		}
		return rows.Err()
	})
	return result, err
}

// RestartedCLIAttempt identifies one restart-interrupted CLI result.
type RestartedCLIAttempt struct {
	ProjectID   string
	AttemptID   string
	Revision    int64
	HasEvidence bool
}

// SettleRestartedCLI fails one restart-interrupted CLI attempt: the restart is
// the proof its process ended. Retained evidence stays, no credit is granted,
// and the PR reservation is released so the PR can be reviewed again.
func (e *Engine) SettleRestartedCLI(ctx context.Context, x RestartedCLIAttempt) (Attempt, error) {
	if !e.restartEndsCLI {
		return Attempt{}, conflict()
	}
	return e.advanceGuarded(ctx, x.ProjectID, x.AttemptID, x.Revision, "failed", CLIRestartSettlementReason, "", nil, func(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) error {
		var eligible bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations r WHERE r.attempt_id=? AND r.action_class='cli' AND r.execution_ended_at<>'') AND NOT EXISTS(SELECT 1 FROM review_execution_evidence v WHERE v.attempt_id=? AND json_valid(v.body_json) AND json_extract(v.body_json,'$.capture.review.Complete')=1 AND json_extract(v.body_json,'$.capture.review.Outcome')='completed')`, a.ID, a.ID).Scan(&eligible)
		if err != nil {
			return err
		}
		if !eligible || o.Channel != "cli" || o.State != "uncertain" || o.Reason != "supervisor_interrupted" || o.AttemptID != a.ID || a.State != "uncertain" {
			return conflict()
		}
		return nil
	})
}
