package reviewexec

import (
	"context"
	"database/sql"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

// RecordCLIProcessExit is a trusted native-runtime boundary, not a public
// settlement API. It frees only execution capacity. The reservation continues
// to exclude another run on this PR until its unresolved result is settled.
func (e *Engine) RecordCLIProcessExit(ctx context.Context, project, attemptID string, ended time.Time) error {
	if ended.IsZero() || ended.After(time.Now().Add(5*time.Second)) {
		return invalid()
	}
	return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		if o.Channel != "cli" || o.AttemptID != a.ID || o.State != a.State || (a.State != "active" && a.State != "uncertain") || a.Anchor == nil || a.ExternalID != "" {
			return conflict()
		}
		var admitted string
		if err := tx.QueryRowContext(ctx, `SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND state='active' ORDER BY revision LIMIT 1`, a.ID).Scan(&admitted); err != nil {
			return err
		}
		at, err := time.Parse(time.RFC3339Nano, admitted)
		if err != nil || ended.Before(at) {
			return invalid()
		}
		result, err := tx.ExecContext(ctx, `UPDATE review_execution_reservations SET execution_ended_at=? WHERE attempt_id=? AND provider=? AND account=? AND action_class='cli' AND repository_id=? AND pull_number=? AND execution_ended_at=''`, ended.UTC().Format(time.RFC3339Nano), a.ID, a.Provider, a.Account, a.Anchor.RepositoryID, o.PullNumber)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: project, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, "review.cli_process_exited", a.ID, a.Revision, struct{}{})
	})
}
