package server

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

// checkWorkerNameAvailable must run inside the same serialized mutation as the write.
func checkWorkerNameAvailable(ctx context.Context, tx *sql.Tx, projectID, name string) error {
	var duplicate bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workers w
		WHERE w.project_id=? AND w.name=? AND NOT EXISTS(
			SELECT 1 FROM worker_retirements r WHERE r.project_id=w.project_id AND r.worker_id=w.id))`, projectID, name).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return core.Fail("worker_already_configured", "An active worker with this name already exists in the selected project.", http.StatusConflict)
	}
	return nil
}

func (a *api) renameWorker(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var request struct {
		ExpectedName string `json:"expected_name"`
		Name         string `json:"name"`
	}
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	if !boundedText(request.Name, 200) || !boundedText(request.ExpectedName, 200) {
		core.WriteError(w, core.Fail("validation_failed", "Provide the current worker name and a new name without surrounding whitespace (up to 200 bytes each).", 400))
		return
	}
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("worker_id")
	result, err := a.store.Mutate(r.Context(), scope, core.Mutation{Operation: "workers.rename", Target: id, Key: k, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := core.CheckActiveWorker(ctx, tx, scope.ProjectID, id); err != nil {
			return nil, err
		}
		var current string
		if err := tx.QueryRowContext(ctx, "SELECT name FROM workers WHERE project_id=? AND id=?", scope.ProjectID, id).Scan(&current); err != nil {
			return nil, err
		}
		if current != request.ExpectedName {
			return nil, core.Fail("worker_name_changed", "The worker name changed. Review the current name and try again.", 409)
		}
		receipt := map[string]string{"id": id, "name": request.Name}
		if current == request.Name {
			return receipt, nil
		}
		if err := checkWorkerNameAvailable(ctx, tx, scope.ProjectID, request.Name); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE workers SET name=? WHERE project_id=? AND id=?", request.Name, scope.ProjectID, id); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "workers.renamed", id, 1, map[string]string{"id": id}); err != nil {
			return nil, err
		}
		return receipt, nil
	})
	complete(w, result, err)
}
