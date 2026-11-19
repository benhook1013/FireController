package reviewexec

import (
	"context"
	"database/sql"
	"math"

	"github.com/benhook1013/FireController/internal/core"
)

// recoverUnqueriedHostedRetry preserves a validated existing cooldown intent
// when preparation stopped before retaining any provider query. It grants no
// allowance: the next attempt still rechecks source and obtains fresh evidence.
func (e *Engine) recoverUnqueriedHostedRetry(ctx context.Context, tx *sql.Tx, o Operation, a Attempt) (bool, error) {
	if o.Channel != "hosted" || o.State != "preparing" || o.AttemptID != a.ID || a.State != "preparing" || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || o.Wait == nil || o.Wait.BaselineAttemptID == "" || o.Wait.BaselineAttemptID == a.ID || o.Wait.RetryNotBefore == "" {
		return false, nil
	}
	// readOperation has already validated the retained wait and historical
	// cooldown authority. Raw row existence, not decoding success, fences any
	// possible provider effect, including malformed or incomplete query records.
	for _, table := range []string{"review_hosted_allowance_queries", "review_hosted_observations", "review_hosted_preflight", "review_execution_evidence"} {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE attempt_id=?", a.ID).Scan(&count); err != nil {
			return false, err
		}
		if count != 0 {
			return false, nil
		}
	}
	if a.Revision == math.MaxInt64 || o.Revision == math.MaxInt64 {
		return false, conflict()
	}
	at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
	if err != nil {
		return false, err
	}
	a.State, a.Revision = "cancelled", a.Revision+1
	if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state='cancelled',revision=? WHERE id=?`, a.Revision, a.ID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='cancelled',reason='supervisor_interrupted',revision=revision+1,updated_at=? WHERE id=?`, at, o.ID); err != nil {
		return false, err
	}
	if err = e.transitionRow(ctx, tx, a, "supervisor_interrupted", at); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='queued',reason='',attempt_id='' WHERE id=?`, o.ID); err != nil {
		return false, err
	}
	if err = core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
		return false, err
	}
	return true, nil
}
