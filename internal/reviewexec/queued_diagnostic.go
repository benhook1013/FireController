package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

const queuedDiagnosticHeader = "X-FC-Review-Queued"

// A best-effort current scheduling hint, never retained journal state or
// admission authority. Drift or unavailable facts omit the header, not the
// original response/receipt. The exact operation is reauthorized and compared
// with the response revision before consulting any current scheduling facts.
func (e *Engine) queuedResponseHeader(w http.ResponseWriter, r *http.Request, scope core.Scope, id, state string, revision int64) {
	if state != "queued" {
		return
	}
	cause := e.queuedDiagnostic(r.Context(), scope, id, revision)
	if cause != "" {
		w.Header().Set(queuedDiagnosticHeader, cause)
	}
}
func (e *Engine) queuedDiagnostic(ctx context.Context, scope core.Scope, id string, revision int64) string {
	cause := ""
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		op, err := readOperation(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		if op.ID != id || op.State != "queued" || op.Revision != revision {
			return nil
		}
		var originalJSON, latestJSON sql.NullString
		var repositoryID, latestRevision int64
		err = tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(c.body_json AS BLOB))<=32768 THEN c.body_json END,CASE WHEN length(CAST(latest.body_json AS BLOB))<=32768 THEN latest.body_json END,p.github_repository_id,latest.revision FROM review_execution_config c JOIN projects p ON p.id=c.project_id JOIN review_execution_config latest ON latest.project_id=c.project_id AND latest.revision=(SELECT MAX(revision) FROM review_execution_config WHERE project_id=c.project_id) WHERE c.project_id=? AND c.revision=?`, op.ProjectID, op.ConfigurationRevision).Scan(&originalJSON, &latestJSON, &repositoryID, &latestRevision)
		if err != nil {
			return err
		}
		var original, latest Configuration
		if !originalJSON.Valid || !latestJSON.Valid || json.Unmarshal([]byte(originalJSON.String), &original) != nil || json.Unmarshal([]byte(latestJSON.String), &latest) != nil || validateConfigurationShape(original) != nil || validateConfigurationShape(latest) != nil {
			return adjudicationStorageError()
		}
		switch {
		case op.Wait != nil && op.Wait.Paused:
			cause = "operation_paused"
			return nil
		case latest.Paused:
			cause = "project_paused"
			return nil
		case op.Wait != nil && op.Wait.RetryNotBefore != "" && e.sampleExecutionTime().Before(mustWaitTime(op.Wait.RetryNotBefore)):
			cause = "cooldown"
			return nil
		}
		earlier, err := e.earlierQueuedHostedIntent(ctx, tx, op, latest, latestRevision)
		if err != nil {
			return err
		}
		if earlier != nil {
			cause = "queue_order"
			return nil
		}
		cause = "awaiting_scheduler"
		capability, ok := e.capabilities[original.CapabilityID]
		if !ok || capability.Fingerprint != op.CapabilityFingerprint {
			return nil
		}
		var busy bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_reservations WHERE provider=? AND account=? AND action_class=?) OR EXISTS(SELECT 1 FROM review_execution_reservations WHERE provider=? AND repository_id=? AND pull_number=? AND action_class=?)`, capability.Provider, capability.Account, op.Channel, capability.Provider, repositoryID, op.PullNumber, op.Channel).Scan(&busy)
		if busy {
			cause = "reservation_busy"
		}
		return err
	})
	if err != nil {
		return ""
	}
	return cause
}
func (e *Engine) queuedRunResponseHeader(w http.ResponseWriter, r *http.Request, scope core.Scope, state RunState) {
	if state.Operation != nil {
		o := state.Operation
		e.queuedResponseHeader(w, r, scope, o.ID, o.State, o.Revision)
		e.deploymentResponseHeader(w)
	}
}
