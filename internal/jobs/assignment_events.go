package jobs

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/core"
)

// Assignment attention is committed with ownership, without manufacturing an
// inbox message or notifying the worker who just performed the change.
func appendAssignmentEvents(ctx context.Context, tx *sql.Tx, scope core.Scope, previousWorker string, next job) error {
	if previousWorker == next.WorkerID {
		return nil
	}
	for _, change := range []struct{ kind, worker string }{{"job.unassigned", previousWorker}, {"job.assigned", next.WorkerID}} {
		if change.worker == "" || change.worker == scope.WorkerID {
			continue
		}
		payload := struct {
			RecipientWorkerID string `json:"recipient_worker_id"`
			SenderWorkerID    string `json:"sender_worker_id,omitempty"`
		}{change.worker, scope.WorkerID}
		if err := core.AppendEvent(ctx, tx, scope, change.kind, next.ID, next.Revision, payload); err != nil {
			return err
		}
	}
	return nil
}
