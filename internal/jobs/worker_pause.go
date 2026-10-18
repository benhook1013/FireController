package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

type workerPause struct {
	WorkerID string `json:"worker_id"`
	Revision int64  `json:"revision"`
	Paused   bool   `json:"paused"`
	Reason   string `json:"reason"`
}
type workerPauseRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	Paused           *bool   `json:"paused"`
	Reason           *string `json:"reason"`
}
type workerPauseReplacement struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Paused           bool   `json:"paused"`
	Reason           string `json:"reason"`
}
type workerPauseEntry struct {
	WorkerID      string `json:"worker_id"`
	Revision      int64  `json:"revision"`
	Paused        bool   `json:"paused"`
	Reason        string `json:"reason"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	CreatedAt     string `json:"created_at"`
}
type workerPausePage struct {
	Items   []workerPauseEntry `json:"items"`
	Cursor  string             `json:"cursor,omitempty"`
	HasMore bool               `json:"has_more"`
}

func validPauseReason(paused bool, reason string) bool {
	return len(reason) <= 1024 && utf8.ValidString(reason) && (paused || reason == "") && strings.IndexFunc(reason, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) < 0
}
func readWorkerPause(ctx context.Context, tx *sql.Tx, project, worker string) (workerPause, error) {
	result := workerPause{WorkerID: worker}
	var paused int
	err := tx.QueryRowContext(ctx, `SELECT revision,paused,reason FROM worker_pause_state WHERE project_id=? AND worker_id=?`, project, worker).Scan(&result.Revision, &paused, &result.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return workerPause{}, err
	}
	result.Paused = paused == 1
	if result.Revision < 1 || (paused != 0 && paused != 1) || !validPauseReason(result.Paused, result.Reason) {
		return workerPause{}, core.Fail("storage_unavailable", "Stored worker pause is unavailable.", http.StatusInternalServerError)
	}
	return result, nil
}
func getWorkerPause(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		worker := r.PathValue("worker_id")
		if !validPrimaryID(worker) {
			core.WriteError(w, core.Fail("validation_failed", "Select a valid worker identity.", 400))
			return
		}
		var result workerPause
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if err := primaryWorker(ctx, tx, scope.ProjectID, worker); err != nil {
				return err
			}
			result, err = readWorkerPause(ctx, tx, scope.ProjectID, worker)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, result)
	}
}
func replaceWorkerPause(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "write")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		worker := r.PathValue("worker_id")
		if !validPrimaryID(worker) {
			core.WriteError(w, core.Fail("validation_failed", "Select a valid worker identity.", 400))
			return
		}
		var raw workerPauseRequest
		if err = core.ReadJSON(w, r, &raw); err != nil {
			core.WriteError(w, err)
			return
		}
		if raw.ExpectedRevision == nil || *raw.ExpectedRevision < 0 || *raw.ExpectedRevision == math.MaxInt64 {
			core.WriteError(w, core.Fail("invalid_revision", "expected_revision is required and must allow a next revision.", 400))
			return
		}
		if raw.Paused == nil || raw.Reason == nil || !validPauseReason(*raw.Paused, *raw.Reason) {
			core.WriteError(w, core.Fail("validation_failed", "Supply an explicit paused flag and valid reason up to 1024 bytes; resume requires an empty reason.", 400))
			return
		}
		req := workerPauseReplacement{ExpectedRevision: *raw.ExpectedRevision, Paused: *raw.Paused, Reason: *raw.Reason}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "workers.pause", Target: worker, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if err := primaryWorker(ctx, tx, scope.ProjectID, worker); err != nil {
				return nil, err
			}
			current, err := readWorkerPause(ctx, tx, scope.ProjectID, worker)
			if err != nil {
				return nil, err
			}
			if current.Revision != req.ExpectedRevision {
				return nil, &core.Error{Code: "worker_pause_revision_conflict", Message: "Reload the worker pause state before replacing it.", Status: 409, Details: map[string]int64{"current_revision": current.Revision}}
			}
			value := receipt{ID: worker, Revision: current.Revision, Outcome: "unchanged"}
			if current.Paused == req.Paused && current.Reason == req.Reason {
				return value, nil
			}
			revision := current.Revision + 1
			at := timestamp()
			_, err = tx.ExecContext(ctx, `INSERT INTO worker_pause_state(project_id,worker_id,revision,paused,reason) VALUES(?,?,?,?,?) ON CONFLICT(project_id,worker_id) DO UPDATE SET revision=excluded.revision,paused=excluded.paused,reason=excluded.reason`, scope.ProjectID, worker, revision, req.Paused, req.Reason)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO worker_pause_history(project_id,worker_id,revision,paused,reason,actor_id,actor_worker_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, worker, revision, req.Paused, req.Reason, scope.PrincipalID, scope.WorkerID, at)
			if err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "worker.pause_changed", worker, revision, map[string]any{"worker_id": worker, "paused": req.Paused}); err != nil {
				return nil, err
			}
			value.Revision = revision
			value.Outcome = "updated"
			return value, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, json.RawMessage(result))
	}
}
func workerPauseHistory(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "invalid_query", "limit", "cursor"); err != nil {
			core.WriteError(w, err)
			return
		}
		worker := r.PathValue("worker_id")
		if !validPrimaryID(worker) {
			core.WriteError(w, core.Fail("validation_failed", "Select a valid worker identity.", 400))
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "worker-pause:" + scope.ProjectID + ":" + worker + ":revision-desc"
		before, err := progressCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := workerPausePage{Items: make([]workerPauseEntry, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if err := primaryWorker(ctx, tx, scope.ProjectID, worker); err != nil {
				return err
			}
			query := `SELECT worker_id,revision,paused,reason,actor_id,actor_worker_id,created_at FROM worker_pause_history WHERE project_id=? AND worker_id=?`
			args := []any{scope.ProjectID, worker}
			if before > 0 {
				query += ` AND revision<?`
				args = append(args, before)
			}
			query += ` ORDER BY revision DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if len(result.Items) == limit {
					result.HasMore = true
					break
				}
				var entry workerPauseEntry
				var paused int
				if err = rows.Scan(&entry.WorkerID, &entry.Revision, &paused, &entry.Reason, &entry.ActorID, &entry.ActorWorkerID, &entry.CreatedAt); err != nil {
					return err
				}
				entry.Paused = paused == 1
				if entry.Revision < 1 || (paused != 0 && paused != 1) || !validPauseReason(entry.Paused, entry.Reason) {
					return core.Fail("storage_unavailable", "Stored worker pause history is unavailable.", http.StatusInternalServerError)
				}
				result.Items = append(result.Items, entry)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if result.HasMore {
				result.Cursor = store.Cursor(scope, filter, strconv.FormatInt(result.Items[len(result.Items)-1].Revision, 10))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, result, maxProgressHistory)
	}
}
