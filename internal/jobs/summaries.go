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

// Summary is deliberately authored prose. It never derives content from a job.
type Summary struct {
	JobID       string `json:"job_id"`
	JobRevision int64  `json:"job_revision"`
	Revision    int64  `json:"revision"`
	Title       string `json:"title"`
	Body        string `json:"body"`
}

type SummaryRevision struct {
	Summary
	ActorID   string `json:"actor_id"`
	WorkerID  string `json:"worker_id"`
	CreatedAt string `json:"created_at"`
}

type summaryRequest struct {
	ExpectedJobRevision *int64  `json:"expected_job_revision"`
	ExpectedRevision    *int64  `json:"expected_revision"`
	Title               *string `json:"title"`
	Body                *string `json:"body"`
}
type summaryReplacement struct {
	ExpectedJobRevision int64  `json:"expected_job_revision"`
	ExpectedRevision    int64  `json:"expected_revision"`
	Title               string `json:"title"`
	Body                string `json:"body"`
}
type summaryPage struct {
	Items   []SummaryRevision `json:"items"`
	Cursor  string            `json:"cursor,omitempty"`
	HasMore bool              `json:"has_more"`
}

func validSummary(title, body string) bool {
	if title == "" && body == "" {
		return true
	}
	if strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" || len(title) > 240 || len(body) > 2000 || !utf8.ValidString(title) || !validCheckpointText(body, 2000, true) {
		return false
	}
	for _, ch := range title {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func summaryUnavailable() error {
	return core.Fail("storage_unavailable", "Stored job summary is unavailable.", 500)
}

// ReadSummary reads the attached summary in the caller's transaction. Its job
// revision is the current private job revision, independent of summary edits.
func ReadSummary(ctx context.Context, tx *sql.Tx, project, id string) (Summary, error) {
	current, err := readJob(ctx, tx, project, id)
	if err != nil {
		return Summary{}, err
	}
	result := Summary{JobID: id, JobRevision: current.Revision}
	err = tx.QueryRowContext(ctx, `SELECT revision,title,body FROM job_summaries WHERE project_id=? AND job_id=?`, project, id).Scan(&result.Revision, &result.Title, &result.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return Summary{}, err
	}
	if result.Revision < 1 || !validSummary(result.Title, result.Body) {
		return Summary{}, summaryUnavailable()
	}
	return result, nil
}

// ReadSummaryRevision returns exactly one retained authored revision. Public
// publication selects this record explicitly; it never reads a private brief.
func ReadSummaryRevision(ctx context.Context, tx *sql.Tx, project, id string, revision int64) (SummaryRevision, error) {
	var out SummaryRevision
	err := tx.QueryRowContext(ctx, `SELECT job_id,job_revision,revision,title,body,actor_id,worker_id,created_at FROM job_summary_history WHERE project_id=? AND job_id=? AND revision=?`, project, id, revision).Scan(&out.JobID, &out.JobRevision, &out.Revision, &out.Title, &out.Body, &out.ActorID, &out.WorkerID, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, core.Fail("not_found", "The job summary revision is unavailable.", 404)
	}
	if err != nil {
		return out, err
	}
	if !validSummaryRevision(out) {
		return SummaryRevision{}, summaryUnavailable()
	}
	return out, nil
}
func validSummaryRevision(v SummaryRevision) bool {
	return validPrimaryID(v.JobID) && v.JobRevision > 0 && v.Revision > 0 && validSummary(v.Title, v.Body) && validPrimaryID(v.ActorID) && (v.WorkerID == "" || validPrimaryID(v.WorkerID)) && contextTime(v.CreatedAt)
}

func getSummary(store *core.Store) http.HandlerFunc {
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
		var out Summary
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			out, err = ReadSummary(ctx, tx, scope.ProjectID, r.PathValue("job_id"))
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, out)
	}
}

func replaceSummary(store *core.Store) http.HandlerFunc {
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
		var raw summaryRequest
		if err = core.ReadJSON(w, r, &raw); err != nil {
			core.WriteError(w, err)
			return
		}
		if raw.ExpectedJobRevision == nil || *raw.ExpectedJobRevision < 1 || raw.ExpectedRevision == nil || *raw.ExpectedRevision < 0 || *raw.ExpectedRevision == math.MaxInt64 {
			core.WriteError(w, core.Fail("invalid_revision", "Supply the current job and writable summary revisions.", 400))
			return
		}
		if raw.Title == nil || raw.Body == nil || !validSummary(*raw.Title, *raw.Body) {
			core.WriteError(w, core.Fail("validation_failed", "Supply both an explicit title up to 240 bytes and body up to 2000 bytes, or both empty to clear.", 400))
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		req := summaryReplacement{*raw.ExpectedJobRevision, *raw.ExpectedRevision, *raw.Title, *raw.Body}
		id := r.PathValue("job_id")
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "jobs.summary", Target: id, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := ReadSummary(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			if current.JobRevision != req.ExpectedJobRevision {
				return nil, revisionConflict(current.JobRevision)
			}
			if current.Revision != req.ExpectedRevision {
				return nil, &core.Error{Code: "summary_revision_conflict", Message: "Reload the current summary before replacing it.", Status: 409, Details: map[string]any{"current_revision": current.Revision}}
			}
			receipt := checklistReceipt{ID: id, Revision: current.Revision, JobRevision: current.JobRevision, Outcome: "unchanged"}
			if current.Title == req.Title && current.Body == req.Body {
				return receipt, nil
			}
			revision := current.Revision + 1
			at := timestamp()
			args := []any{scope.ProjectID, id, revision, current.JobRevision, req.Title, req.Body, scope.PrincipalID, scope.WorkerID, at}
			_, err = tx.ExecContext(ctx, `INSERT INTO job_summaries(project_id,job_id,revision,job_revision,title,body,actor_id,worker_id,created_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,job_id) DO UPDATE SET revision=excluded.revision,job_revision=excluded.job_revision,title=excluded.title,body=excluded.body,actor_id=excluded.actor_id,worker_id=excluded.worker_id,created_at=excluded.created_at`, args...)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO job_summary_history(project_id,job_id,revision,job_revision,title,body,actor_id,worker_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, args...)
			if err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "job.summary_replaced", id, revision, map[string]any{"job_id": id, "summary_revision": revision}); err != nil {
				return nil, err
			}
			receipt.Revision = revision
			receipt.Outcome = "updated"
			return receipt, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, json.RawMessage(result))
	}
}

func summaryHistory(store *core.Store) http.HandlerFunc {
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
		id := r.PathValue("job_id")
		filter := "job-summary:" + scope.ProjectID + ":" + id + ":revision-desc"
		before, err := progressCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		out := summaryPage{Items: make([]SummaryRevision, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := readJob(ctx, tx, scope.ProjectID, id); err != nil {
				return err
			}
			query := `SELECT job_id,job_revision,revision,title,body,actor_id,worker_id,created_at FROM job_summary_history WHERE project_id=? AND job_id=?`
			args := []any{scope.ProjectID, id}
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
				if len(out.Items) == limit {
					out.HasMore = true
					break
				}
				var item SummaryRevision
				if err := rows.Scan(&item.JobID, &item.JobRevision, &item.Revision, &item.Title, &item.Body, &item.ActorID, &item.WorkerID, &item.CreatedAt); err != nil {
					return err
				}
				if !validSummaryRevision(item) {
					return summaryUnavailable()
				}
				out.Items = append(out.Items, item)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if out.HasMore {
				out.Cursor = store.Cursor(scope, filter, strconv.FormatInt(out.Items[len(out.Items)-1].Revision, 10))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, out, maxProgressHistory)
	}
}
func getSummaryRevision(store *core.Store) http.HandlerFunc {
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
		revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
		if err != nil || revision < 1 {
			core.WriteError(w, core.Fail("invalid_revision", "Use a positive summary revision.", 400))
			return
		}
		var out SummaryRevision
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := readJob(ctx, tx, scope.ProjectID, r.PathValue("job_id")); err != nil {
				return err
			}
			var err error
			out, err = ReadSummaryRevision(ctx, tx, scope.ProjectID, r.PathValue("job_id"), revision)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, out)
	}
}
