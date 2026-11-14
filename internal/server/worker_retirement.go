package server

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type workerRetirementItems struct {
	Count int64    `json:"count"`
	IDs   []string `json:"ids"`
}
type workerRetirementBlockers struct {
	Jobs     workerRetirementItems `json:"jobs"`
	Notes    workerRetirementItems `json:"notes"`
	Messages workerRetirementItems `json:"messages"`
}
type workerRetirementStatus struct {
	WorkerID  string                   `json:"worker_id"`
	Retired   bool                     `json:"retired"`
	CanRetire bool                     `json:"can_retire"`
	Blockers  workerRetirementBlockers `json:"blockers"`
}

func retirementItems(ctx context.Context, tx *sql.Tx, query string, args ...any) (workerRetirementItems, error) {
	items := workerRetirementItems{IDs: []string{}}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY id LIMIT 20`, args...)
	if err != nil {
		return items, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &items.Count); err != nil {
			return items, err
		}
		items.IDs = append(items.IDs, id)
	}
	return items, rows.Err()
}

func readWorkerRetirement(ctx context.Context, tx *sql.Tx, project, worker string) (workerRetirementStatus, error) {
	status := workerRetirementStatus{WorkerID: worker}
	if err := core.CheckWorker(ctx, tx, project, worker); err != nil {
		return status, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_retirements WHERE project_id=? AND worker_id=?)`, project, worker).Scan(&status.Retired); err != nil {
		return status, err
	}
	var err error
	status.Blockers.Jobs, err = retirementItems(ctx, tx, `SELECT id,count(*) OVER() FROM jobs WHERE project_id=? AND worker_id=? AND status<>'completed'`, project, worker)
	if err != nil {
		return status, err
	}
	status.Blockers.Notes, err = retirementItems(ctx, tx, `SELECT n.id,count(*) OVER() FROM notes n WHERE n.project_id=? AND n.status='pending' AND (n.worker_id=? OR EXISTS(SELECT 1 FROM jobs j WHERE j.project_id=n.project_id AND j.id=n.job_id AND j.worker_id=?))`, project, worker, worker)
	if err != nil {
		return status, err
	}
	status.Blockers.Messages, err = retirementItems(ctx, tx, `SELECT m.id,count(*) OVER() FROM inbox_messages m LEFT JOIN inbox_recipient_state r ON r.project_id=m.project_id AND r.message_id=m.id AND r.worker_id=m.recipient_worker_id WHERE m.project_id=? AND m.recipient_worker_id=? AND COALESCE(r.acknowledged,0)=0`, project, worker)
	status.CanRetire = !status.Retired && status.Blockers.Jobs.Count == 0 && status.Blockers.Notes.Count == 0 && status.Blockers.Messages.Count == 0
	return status, err
}

func (a *api) workerRetirement(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var status workerRetirementStatus
	err = a.store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		status, err = readWorkerRetirement(ctx, tx, scope.ProjectID, r.PathValue("worker_id"))
		return err
	})
	complete(w, status, err)
}

func (a *api) retireWorker(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var request struct{}
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("worker_id")
	result, err := a.store.Mutate(r.Context(), scope, core.Mutation{Operation: "workers.retire", Target: id, Key: k, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		status, err := readWorkerRetirement(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		outcome := "unchanged"
		if !status.Retired {
			if !status.CanRetire {
				return nil, &core.Error{Code: "worker_retirement_blocked", Message: "Finish, reassign or consume the listed jobs and notes, and explicitly acknowledge incoming messages before retiring this worker.", Status: 409, Details: status.Blockers}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO worker_retirements(project_id,worker_id,retired_at,actor_id) VALUES(?,?,?,?)`, scope.ProjectID, id, time.Now().UTC().Format(time.RFC3339Nano), scope.PrincipalID); err != nil {
				return nil, err
			}
			if err := core.AppendEvent(ctx, tx, scope, "workers.retired", id, 2, map[string]string{"worker_id": id}); err != nil {
				return nil, err
			}
			outcome = "retired"
		}
		return map[string]any{"id": id, "revision": 2, "outcome": outcome}, nil
	})
	complete(w, result, err)
}
