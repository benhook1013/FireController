package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxProgressHistory = 4 << 20
const maxCurrentChecklist = 128 << 10

type updateRequest struct {
	ExpectedJobRevision int64    `json:"expected_job_revision"`
	Body                string   `json:"body"`
	Evidence            []string `json:"evidence"`
}
type fullUpdate struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Body        string   `json:"body"`
	Evidence    []string `json:"evidence"`
	CreatedAt   string   `json:"created_at"`
}
type updatePage struct {
	Items   []fullUpdate `json:"items"`
	Cursor  string       `json:"cursor,omitempty"`
	HasMore bool         `json:"has_more"`
}

func validateUpdate(req updateRequest) error {
	if req.ExpectedJobRevision < 1 {
		return core.Fail("invalid_revision", "expected_job_revision must be positive.", 400)
	}
	if !validCheckpointText(req.Body, 8<<10, true) || len(req.Evidence) > 10 {
		return core.Fail("validation_failed", "Use a nonblank body up to 8192 bytes and at most ten evidence references.", 400)
	}
	size := len(req.Body)
	for _, ref := range req.Evidence {
		if !validCheckpointText(ref, 1024, true) {
			return core.Fail("validation_failed", "Each evidence reference must be nonblank text up to 1024 bytes.", 400)
		}
		size += len(ref)
	}
	if size > 16<<10 {
		return core.Fail("validation_failed", "Update content exceeds 16384 bytes.", 400)
	}
	return nil
}
func appendUpdate(store *core.Store) http.HandlerFunc {
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
		var req updateRequest
		if err = core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if req.Evidence == nil {
			req.Evidence = []string{}
		}
		if err = validateUpdate(req); err != nil {
			core.WriteError(w, err)
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		jobID := r.PathValue("job_id")
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "jobs.update", Target: jobID, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := readJob(ctx, tx, scope.ProjectID, jobID)
			if err != nil {
				return nil, err
			}
			if current.Revision != req.ExpectedJobRevision {
				return nil, revisionConflict(current.Revision)
			}
			evidence, err := json.Marshal(req.Evidence)
			if err != nil {
				return nil, err
			}
			id := core.NewID("job_update")
			var sequence int64
			err = tx.QueryRowContext(ctx, `INSERT INTO job_updates(id,project_id,job_id,job_revision,actor_id,worker_id,body,evidence_json,created_at) VALUES(?,?,?,?,?,?,?,?,?) RETURNING sequence`, id, scope.ProjectID, jobID, current.Revision, scope.PrincipalID, scope.WorkerID, req.Body, string(evidence), timestamp()).Scan(&sequence)
			if err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "job.update_appended", id, 1, map[string]any{"job_id": jobID, "sequence": sequence}); err != nil {
				return nil, err
			}
			return checkpointReceipt{ID: id, Sequence: sequence, JobRevision: current.Revision, Outcome: "created"}, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, json.RawMessage(result))
	}
}
func updateHistory(store *core.Store) http.HandlerFunc {
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
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		jobID := r.PathValue("job_id")
		filter := "job-updates:" + scope.ProjectID + ":" + jobID + ":sequence-desc"
		before, err := progressCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		response := updatePage{Items: make([]fullUpdate, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := readJob(ctx, tx, scope.ProjectID, jobID); err != nil {
				return err
			}
			query := `SELECT id,project_id,job_id,job_revision,sequence,actor_id,worker_id,body,evidence_json,created_at FROM job_updates WHERE project_id=? AND job_id=?`
			args := []any{scope.ProjectID, jobID}
			if before > 0 {
				query += ` AND sequence<?`
				args = append(args, before)
			}
			query += ` ORDER BY sequence DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if len(response.Items) == limit {
					response.HasMore = true
					break
				}
				var item fullUpdate
				var evidence string
				if err = rows.Scan(&item.ID, &item.ProjectID, &item.JobID, &item.JobRevision, &item.Sequence, &item.ActorID, &item.WorkerID, &item.Body, &evidence, &item.CreatedAt); err != nil {
					return err
				}
				if strictjson.Decode([]byte(evidence), &item.Evidence) != nil || item.Evidence == nil || validateUpdate(updateRequest{ExpectedJobRevision: item.JobRevision, Body: item.Body, Evidence: item.Evidence}) != nil {
					return core.Fail("storage_unavailable", "Stored job update is unavailable.", http.StatusInternalServerError)
				}
				response.Items = append(response.Items, item)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if response.HasMore {
				response.Cursor = store.Cursor(scope, filter, strconv.FormatInt(response.Items[len(response.Items)-1].Sequence, 10))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, response, maxProgressHistory)
	}
}
func progressCursor(store *core.Store, scope core.Scope, filter string, r *http.Request) (int64, error) {
	value, err := store.ParseCursor(scope, filter, r.URL.Query().Get("cursor"))
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != value {
		return 0, core.Fail("validation_failed", "Use a cursor from this progress history.", 400)
	}
	return n, nil
}
func writeProgressJSON(w http.ResponseWriter, value any, limit int) {
	data, err := json.Marshal(value)
	if err != nil {
		core.WriteError(w, core.Fail("internal_error", "The job progress response is unavailable.", 500))
		return
	}
	if len(data)+1 > limit {
		core.WriteError(w, core.Fail("response_too_large", "The progress response exceeds its encoded limit; request a smaller history page.", 413))
		return
	}
	core.WriteJSON(w, http.StatusOK, json.RawMessage(data))
}
