package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

type primaryJob struct {
	WorkerID string `json:"worker_id"`
	Revision int64  `json:"revision"`
	JobID    string `json:"job_id"`
}
type primaryJobRequest struct {
	ExpectedRevision *int64  `json:"expected_revision"`
	JobID            *string `json:"job_id"`
}
type primaryJobReplacement struct {
	ExpectedRevision int64  `json:"expected_revision"`
	JobID            string `json:"job_id"`
}
type primaryJobEntry struct {
	WorkerID      string `json:"worker_id"`
	Revision      int64  `json:"revision"`
	JobID         string `json:"job_id"`
	ActorID       string `json:"actor_id"`
	ActorWorkerID string `json:"actor_worker_id"`
	Reason        string `json:"reason"`
	CreatedAt     string `json:"created_at"`
}
type primaryJobPage struct {
	Items   []primaryJobEntry `json:"items"`
	Cursor  string            `json:"cursor,omitempty"`
	HasMore bool              `json:"has_more"`
}

func validPrimaryID(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || value == "." || value == ".." || strings.ContainsAny(value, "/\\%?#") {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
func primaryWorker(ctx context.Context, tx *sql.Tx, project, worker string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM workers WHERE project_id=? AND id=?`, project, worker).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Fail("worker_not_found", "The target worker is unavailable in this project.", 404)
	}
	return err
}
func primaryAssignment(ctx context.Context, tx *sql.Tx, project, worker, job string) error {
	var assigned string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(worker_id,'') FROM jobs WHERE project_id=? AND id=?`, project, job).Scan(&assigned)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Fail("job_not_found", "The selected job is unavailable in this project.", 404)
	}
	if err != nil {
		return err
	}
	if assigned != worker {
		return core.Fail("primary_job_assignment_conflict", "Select a job currently assigned to the target worker.", 409)
	}
	return nil
}
func readPrimaryJob(ctx context.Context, tx *sql.Tx, project, worker string) (primaryJob, error) {
	value := primaryJob{WorkerID: worker}
	err := tx.QueryRowContext(ctx, `SELECT revision,COALESCE(job_id,'') FROM worker_primary_jobs WHERE project_id=? AND worker_id=?`, project, worker).Scan(&value.Revision, &value.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	if err != nil {
		return primaryJob{}, err
	}
	if value.Revision < 1 || value.JobID != "" && !validPrimaryID(value.JobID) {
		return primaryJob{}, core.Fail("storage_unavailable", "Stored worker primary job is unavailable.", http.StatusInternalServerError)
	}
	if value.JobID != "" {
		if err = primaryAssignment(ctx, tx, project, worker, value.JobID); err != nil {
			var failure *core.Error
			if errors.As(err, &failure) {
				return primaryJob{}, core.Fail("storage_unavailable", "Stored worker primary assignment is unavailable.", http.StatusInternalServerError)
			}
			return primaryJob{}, fmt.Errorf("read retained worker primary assignment: %w", err)
		}
	}
	return value, nil
}
func getPrimaryJob(store *core.Store) http.HandlerFunc {
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
		var result primaryJob
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if err := primaryWorker(ctx, tx, scope.ProjectID, worker); err != nil {
				return err
			}
			result, err = readPrimaryJob(ctx, tx, scope.ProjectID, worker)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, result)
	}
}
func replacePrimaryJob(store *core.Store) http.HandlerFunc {
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
		var raw primaryJobRequest
		if err = core.ReadJSON(w, r, &raw); err != nil {
			core.WriteError(w, err)
			return
		}
		if raw.ExpectedRevision == nil || *raw.ExpectedRevision < 0 || *raw.ExpectedRevision == math.MaxInt64 {
			core.WriteError(w, core.Fail("invalid_revision", "expected_revision is required and must allow a next revision.", 400))
			return
		}
		if raw.JobID == nil || *raw.JobID != "" && !validPrimaryID(*raw.JobID) {
			core.WriteError(w, core.Fail("validation_failed", "job_id must be an explicit valid identity or an empty string to clear.", 400))
			return
		}
		req := primaryJobReplacement{ExpectedRevision: *raw.ExpectedRevision, JobID: *raw.JobID}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "workers.primary-job", Target: worker, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if err := primaryWorker(ctx, tx, scope.ProjectID, worker); err != nil {
				return nil, err
			}
			current, err := readPrimaryJob(ctx, tx, scope.ProjectID, worker)
			if err != nil {
				return nil, err
			}
			if current.Revision != req.ExpectedRevision {
				return nil, &core.Error{Code: "primary_job_revision_conflict", Message: "Reload the worker primary before replacing it.", Status: 409, Details: map[string]int64{"current_revision": current.Revision}}
			}
			if req.JobID != "" {
				if err = primaryAssignment(ctx, tx, scope.ProjectID, worker, req.JobID); err != nil {
					return nil, err
				}
			}
			value := receipt{ID: worker, Revision: current.Revision, Outcome: "unchanged"}
			if current.JobID == req.JobID {
				return value, nil
			}
			reason := "selected"
			if req.JobID == "" {
				reason = "cleared"
			}
			revision, err := writePrimaryJob(ctx, tx, scope, current, req.JobID, reason)
			if err != nil {
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

// writePrimaryJob is shared by explicit replacements and atomic assignment clears.
// The caller owns the current-state read and every effect in the same transaction.
func writePrimaryJob(ctx context.Context, tx *sql.Tx, scope core.Scope, current primaryJob, job, reason string) (int64, error) {
	if current.Revision == math.MaxInt64 {
		return 0, core.Fail("primary_job_revision_exhausted", "The current primary revision cannot advance.", 409)
	}
	revision := current.Revision + 1
	at := timestamp()
	_, err := tx.ExecContext(ctx, `INSERT INTO worker_primary_jobs(project_id,worker_id,revision,job_id) VALUES(?,?,?,NULLIF(?,'')) ON CONFLICT(project_id,worker_id) DO UPDATE SET revision=excluded.revision,job_id=excluded.job_id`, scope.ProjectID, current.WorkerID, revision, job)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO worker_primary_job_history(project_id,worker_id,revision,job_id,actor_id,actor_worker_id,reason,created_at) VALUES(?,?,?,NULLIF(?,''),?,?,?,?)`, scope.ProjectID, current.WorkerID, revision, job, scope.PrincipalID, scope.WorkerID, reason, at)
	if err != nil {
		return 0, err
	}
	err = core.AppendEvent(ctx, tx, scope, "worker.primary_job_changed", current.WorkerID, revision, map[string]string{"worker_id": current.WorkerID, "job_id": job, "reason": reason})
	return revision, err
}
func clearReassignedPrimary(ctx context.Context, tx *sql.Tx, scope core.Scope, current, next job) error {
	if current.WorkerID == "" || current.WorkerID == next.WorkerID {
		return nil
	}
	primary, err := readPrimaryJob(ctx, tx, scope.ProjectID, current.WorkerID)
	if err != nil {
		return err
	}
	if primary.JobID != current.ID {
		return nil
	}
	_, err = writePrimaryJob(ctx, tx, scope, primary, "", "job_reassigned")
	return err
}
func primaryJobHistory(store *core.Store) http.HandlerFunc {
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
		filter := "worker-primary-job:" + scope.ProjectID + ":" + worker + ":revision-desc"
		before, err := progressCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := primaryJobPage{Items: make([]primaryJobEntry, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if err := primaryWorker(ctx, tx, scope.ProjectID, worker); err != nil {
				return err
			}
			query := `SELECT worker_id,revision,COALESCE(job_id,''),actor_id,actor_worker_id,reason,created_at FROM worker_primary_job_history WHERE project_id=? AND worker_id=?`
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
				var entry primaryJobEntry
				if err = rows.Scan(&entry.WorkerID, &entry.Revision, &entry.JobID, &entry.ActorID, &entry.ActorWorkerID, &entry.Reason, &entry.CreatedAt); err != nil {
					return err
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
