// Package jobs implements the scoped, durable basic-jobs API.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

const (
	maxTitle = 240
	maxBrief = 32 * 1024
)

var validStatuses = map[string]bool{
	"active": true, "parked": true, "blocked": true, "completed": true,
}

type registration struct {
	publicProjection func(context.Context, *sql.Tx, string, string) error
}

type Option func(*registration)

// WithPublicProjection supplies the optional publication owner's transactional
// refresh. Only assignment/status changes can affect approved job cards.
func WithPublicProjection(refresh func(context.Context, *sql.Tx, string, string) error) Option {
	return func(r *registration) { r.publicProjection = refresh }
}

// Register mounts the basic jobs API on the shared project API mux.
func Register(mux *http.ServeMux, store *core.Store, options ...Option) {
	var config registration
	for _, option := range options {
		option(&config)
	}
	mux.HandleFunc("POST /api/v1/projects/{project_id}/notes", createNote(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/notes", listNotes(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/notes/{note_id}", getNote(store))
	mux.HandleFunc("PATCH /api/v1/projects/{project_id}/notes/{note_id}", patchNote(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/notes/{note_id}/history", noteHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/notes/{note_id}/history/{revision}", getNoteRevision(store))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/jobs", createJob(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs", listJobs(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/delivery", delivery(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}", getJob(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/summary", getSummary(store))
	mux.HandleFunc("PUT /api/v1/projects/{project_id}/jobs/{job_id}/summary", replaceSummary(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/summary/history", summaryHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/summary/history/{revision}", getSummaryRevision(store))
	mux.HandleFunc("PATCH /api/v1/projects/{project_id}/jobs/{job_id}", editJob(store, config.publicProjection))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/history", jobHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/progress/history", currentProgressHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/updates", updateHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/checklist", getChecklist(store))
	mux.HandleFunc("PUT /api/v1/projects/{project_id}/jobs/{job_id}/checklist", replaceChecklist(store))
	mux.HandleFunc("PATCH /api/v1/projects/{project_id}/jobs/{job_id}/checklist/items/{item_id}", updateChecklistItem(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/checklist/history", checklistHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/checkpoints/latest", latestCheckpoint(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/jobs/{job_id}/checkpoints", checkpointHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/workers/{worker_id}/primary-job", getPrimaryJob(store))
	mux.HandleFunc("PUT /api/v1/projects/{project_id}/workers/{worker_id}/primary-job", replacePrimaryJob(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/workers/{worker_id}/primary-job/history", primaryJobHistory(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/workers/{worker_id}/pause", getWorkerPause(store))
	mux.HandleFunc("PUT /api/v1/projects/{project_id}/workers/{worker_id}/pause", replaceWorkerPause(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/workers/{worker_id}/pause/history", workerPauseHistory(store))
}

type job struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	Brief     string `json:"brief"`
	WorkerID  string `json:"worker_id"`
	Status    string `json:"status"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type compactJob struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	WorkerID          string `json:"worker_id"`
	Status            string `json:"status"`
	Revision          int64  `json:"revision"`
	ProgressSummary   string `json:"progress_summary"`
	Blocker           string `json:"blocker"`
	ProgressTruncated bool   `json:"progress_truncated"`
	BlockerTruncated  bool   `json:"blocker_truncated"`
}

type createRequest struct {
	Title    string `json:"title"`
	Brief    string `json:"brief"`
	WorkerID string `json:"worker_id"`
	Status   string `json:"status"`
}

type editRequest struct {
	ExpectedRevision int64          `json:"expected_revision"`
	Progress         *progressPatch `json:"progress,omitempty"`
	Title            *string        `json:"title"`
	Brief            *string        `json:"brief"`
	WorkerID         *string        `json:"worker_id"`
	Status           *string        `json:"status"`
}

type receipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type page struct {
	Items   []compactJob `json:"items"`
	Cursor  string       `json:"cursor,omitempty"`
	HasMore bool         `json:"has_more"`
}

type historyEntry struct {
	Revision  int64           `json:"revision"`
	ActorID   string          `json:"actor_id"`
	State     json.RawMessage `json:"state"`
	CreatedAt string          `json:"created_at"`
}

type historyPage struct {
	Items   []historyEntry `json:"items"`
	Cursor  string         `json:"cursor,omitempty"`
	HasMore bool           `json:"has_more"`
}

func createJob(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "write")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req createRequest
		if err := core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		req.Title = strings.TrimSpace(req.Title)
		req.WorkerID = strings.TrimSpace(req.WorkerID)
		if req.Status == "" {
			req.Status = "active"
		}
		if err := validateContent(req.Title, req.Brief, req.Status); err != nil {
			core.WriteError(w, err)
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{
			Operation: "jobs.create", Target: "jobs", Key: key, Request: req,
		}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if req.WorkerID != "" {
				if err := core.CheckWorker(ctx, tx, scope.ProjectID, req.WorkerID); err != nil {
					return nil, err
				}
			}
			now := timestamp()
			created := job{ID: core.NewID("job"), ProjectID: scope.ProjectID, Title: req.Title,
				Brief: req.Brief, WorkerID: req.WorkerID, Status: req.Status, Revision: 1,
				CreatedAt: now, UpdatedAt: now}
			if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
				(project_id,id,title,brief,worker_id,status,revision,created_at,updated_at)
				VALUES(?,?,?,?,NULLIF(?,''),?,1,?,?)`, created.ProjectID, created.ID, created.Title,
				created.Brief, created.WorkerID, created.Status, now, now); err != nil {
				return nil, err
			}
			if err := seedEmptyProgress(ctx, tx, created); err != nil {
				return nil, err
			}
			if err := appendHistory(ctx, tx, scope, created); err != nil {
				return nil, err
			}
			if err := core.AppendEvent(ctx, tx, scope, "job.created", created.ID, 1,
				map[string]string{"status": created.Status, "worker_id": created.WorkerID}); err != nil {
				return nil, err
			}
			return receipt{ID: created.ID, Revision: 1, Outcome: "created"}, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, json.RawMessage(result))
	}
}

func listJobs(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "invalid_query", "limit", "cursor", "worker_id", "status"); err != nil {
			core.WriteError(w, err)
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
		status := r.URL.Query().Get("status")
		if status != "" && status != "open" && !validStatuses[status] {
			core.WriteError(w, core.Fail("invalid_status", "status is not supported", http.StatusBadRequest))
			return
		}
		filter, err := json.Marshal([]string{scope.ProjectID, workerID, status})
		if err != nil {
			core.WriteError(w, core.Fail("invalid_filter", "job filter is invalid", http.StatusBadRequest))
			return
		}
		filterKey := string(filter)
		if status == "open" {
			filterKey = "jobs-open-v1:" + filterKey
		}
		after, err := store.ParseCursor(scope, filterKey, r.URL.Query().Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query := ""
		var args []any
		if status == "open" {
			afterStatus, afterID := "", ""
			if after != "" {
				var position []string
				if json.Unmarshal([]byte(after), &position) != nil || len(position) != 2 || !validStatuses[position[0]] || position[0] == "completed" || position[1] == "" || len(position[1]) > 128 || !utf8.ValidString(position[1]) || strings.TrimSpace(position[1]) != position[1] || strings.IndexFunc(position[1], unicode.IsControl) >= 0 {
					core.WriteError(w, core.Fail("invalid_cursor", "Use a valid cursor from this open job list.", http.StatusBadRequest))
					return
				}
				afterStatus, afterID = position[0], position[1]
			}
			var branches []string
			for _, selectedStatus := range []string{"active", "blocked", "parked"} {
				if selectedStatus < afterStatus {
					continue
				}
				branch := `SELECT id,title,COALESCE(worker_id,'') AS worker_id,status,revision,project_id FROM jobs WHERE project_id=? AND status=?`
				args = append(args, scope.ProjectID, selectedStatus)
				if selectedStatus == afterStatus {
					branch += ` AND id>?`
					args = append(args, afterID)
				}
				if workerID != "" {
					branch += ` AND worker_id=?`
					args = append(args, workerID)
				}
				branch += ` ORDER BY id LIMIT ?`
				args = append(args, limit+1)
				branches = append(branches, `SELECT * FROM (`+branch+`)`)
			}
			// Bound each indexed status seek before merging, including when a
			// page crosses statuses. Completed jobs never enter the candidates.
			query = `SELECT * FROM (` + strings.Join(branches, ` UNION ALL `) + `) ORDER BY status,id LIMIT ?`
			args = append(args, limit+1)
		} else {
			query = `SELECT id,title,COALESCE(worker_id,'') AS worker_id,status,revision,project_id FROM jobs WHERE project_id=? AND id>?`
			args = []any{scope.ProjectID, after}
			if workerID != "" {
				query += ` AND worker_id=?`
				args = append(args, workerID)
			}
			if status != "" {
				query += ` AND status=?`
				args = append(args, status)
			}
			query += ` ORDER BY id LIMIT ?`
			args = append(args, limit+1)
		}
		query = `WITH selected AS (` + query + `) SELECT s.id,s.title,s.worker_id,s.status,s.revision,CASE WHEN typeof(p.summary)='text' AND length(CAST(p.summary AS BLOB))<=8192 THEN substr(p.summary,1,244) END,CASE WHEN typeof(p.blocker)='text' AND length(CAST(p.blocker AS BLOB))<=4096 THEN substr(p.blocker,1,244) END,p.job_revision FROM selected s LEFT JOIN job_progress p ON p.project_id=s.project_id AND p.job_id=s.id`
		if status == "open" {
			query += ` ORDER BY s.status,s.id`
		} else {
			query += ` ORDER BY s.id`
		}
		rows, err := store.DB().QueryContext(r.Context(), query, args...)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		defer rows.Close()
		result := page{Items: make([]compactJob, 0, limit)}
		lastID, lastStatus := "", ""
		for rows.Next() {
			var item compactJob
			var summary, blocker sql.NullString
			var progressRevision sql.NullInt64
			if err := rows.Scan(&item.ID, &item.Title, &item.WorkerID, &item.Status, &item.Revision, &summary, &blocker, &progressRevision); err != nil {
				core.WriteError(w, err)
				return
			}
			if len(result.Items) == limit {
				result.HasMore = true
				break
			}
			if !summary.Valid || !blocker.Valid || !progressRevision.Valid || progressRevision.Int64 < 1 || progressRevision.Int64 > item.Revision || !utf8.ValidString(summary.String) || !utf8.ValidString(blocker.String) {
				core.WriteError(w, progressUnavailable())
				return
			}
			item.ProgressSummary, item.ProgressTruncated = deliveryPreview(summary.String)
			item.Blocker, item.BlockerTruncated = deliveryPreview(blocker.String)
			lastID, lastStatus = item.ID, item.Status
			result.Items = append(result.Items, item)
		}
		if err := rows.Err(); err != nil {
			core.WriteError(w, err)
			return
		}
		if result.HasMore && lastID != "" {
			after := lastID
			if status == "open" {
				position, _ := json.Marshal([]string{lastStatus, lastID})
				after = string(position)
			}
			result.Cursor = store.Cursor(scope, filterKey, after)
		}
		core.WriteJSON(w, http.StatusOK, result)
	}
}

func getJob(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		var item currentJob
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			item, err = readCurrentJob(ctx, tx, store, scope, r.PathValue("job_id"))
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, item, maxJobContext)
	}
}

func editJob(store *core.Store, projection func(context.Context, *sql.Tx, string, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "write")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "invalid_query"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req editRequest
		if err := core.ReadJSONLimit(w, r, &req, 384<<10); err != nil {
			core.WriteError(w, err)
			return
		}
		normalizeEdit(&req)
		if err := validateEdit(req); err != nil {
			core.WriteError(w, err)
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		jobID := r.PathValue("job_id")
		result, err := store.Mutate(r.Context(), scope, core.Mutation{
			Operation: "jobs.edit", Target: jobID, Key: key, Request: req,
		}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := readJob(ctx, tx, scope.ProjectID, jobID)
			if err != nil {
				return nil, err
			}
			if current.Revision != req.ExpectedRevision {
				return nil, revisionConflict(current.Revision)
			}
			previousProgress, _, err := readProgress(ctx, tx, scope.ProjectID, jobID, current.Revision)
			if err != nil {
				return nil, err
			}
			nextProgress := applyProgressPatch(previousProgress, req.Progress)
			if !validProgress(nextProgress) {
				return nil, core.Fail("validation_failed", "Progress exceeds its content limits.", 400)
			}
			next := current
			if req.Title != nil {
				next.Title = *req.Title
			}
			if req.Brief != nil {
				next.Brief = *req.Brief
			}
			if req.WorkerID != nil {
				next.WorkerID = *req.WorkerID
			}
			if req.Status != nil {
				next.Status = *req.Status
			}
			if next.WorkerID != "" {
				if err := core.CheckWorker(ctx, tx, scope.ProjectID, next.WorkerID); err != nil {
					return nil, err
				}
			}
			if next == current && reflect.DeepEqual(previousProgress, nextProgress) {
				return receipt{ID: current.ID, Revision: current.Revision, Outcome: "unchanged"}, nil
			}
			if current.Revision == math.MaxInt64 {
				return nil, revisionConflict(current.Revision)
			}
			if err := clearReassignedPrimary(ctx, tx, scope, current, next); err != nil {
				return nil, err
			}
			next.Revision++
			next.UpdatedAt = timestamp()
			res, err := tx.ExecContext(ctx, `UPDATE jobs SET title=?,brief=?,worker_id=NULLIF(?,''),status=?,revision=?,updated_at=?
				WHERE project_id=? AND id=? AND revision=?`, next.Title, next.Brief, next.WorkerID, next.Status,
				next.Revision, next.UpdatedAt, scope.ProjectID, jobID, current.Revision)
			if err != nil {
				return nil, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return nil, err
			}
			if n != 1 {
				latest, err := readJob(ctx, tx, scope.ProjectID, jobID)
				if err != nil {
					return nil, err
				}
				return nil, revisionConflict(latest.Revision)
			}
			if !reflect.DeepEqual(previousProgress, nextProgress) {
				if err := storeProgress(ctx, tx, next, nextProgress, core.NewID("job_progress"), scope.PrincipalID, scope.WorkerID, next.UpdatedAt); err != nil {
					return nil, err
				}
			}
			if err := appendHistory(ctx, tx, scope, next); err != nil {
				return nil, err
			}
			if err := core.AppendEvent(ctx, tx, scope, "job.updated", next.ID, next.Revision,
				map[string]string{"status": next.Status, "worker_id": next.WorkerID}); err != nil {
				return nil, err
			}
			if (current.WorkerID != next.WorkerID || current.Status != next.Status) && projection != nil {
				if err := projection(ctx, tx, scope.ProjectID, jobID); err != nil {
					return nil, err
				}
			}
			return receipt{ID: next.ID, Revision: next.Revision, Outcome: "updated"}, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, json.RawMessage(result))
	}
}

func jobHistory(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "invalid_query", "limit", "cursor"); err != nil {
			core.WriteError(w, err)
			return
		}
		jobID := r.PathValue("job_id")
		if _, err := readJob(r.Context(), store.DB(), scope.ProjectID, jobID); err != nil {
			core.WriteError(w, err)
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "job-history:" + scope.ProjectID + ":" + jobID
		after, err := store.ParseCursor(scope, filter, r.URL.Query().Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var afterRevision int64
		if after != "" {
			afterRevision, err = strconv.ParseInt(after, 10, 64)
			if err != nil || afterRevision < 1 {
				core.WriteError(w, core.Fail("invalid_cursor", "Use a valid cursor from this job history.", http.StatusBadRequest))
				return
			}
		}
		query := `SELECT revision,actor_id,state_json,created_at FROM job_history WHERE project_id=? AND job_id=? AND revision>? ORDER BY revision LIMIT ?`
		rows, err := store.DB().QueryContext(r.Context(), query, scope.ProjectID, jobID, afterRevision, limit+1)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		defer rows.Close()
		result := historyPage{Items: make([]historyEntry, 0, limit)}
		var last int64
		for rows.Next() {
			var item historyEntry
			var state string
			if err := rows.Scan(&item.Revision, &item.ActorID, &state, &item.CreatedAt); err != nil {
				core.WriteError(w, err)
				return
			}
			item.State = json.RawMessage(state)
			if len(result.Items) == limit {
				result.HasMore = true
				break
			}
			last = item.Revision
			result.Items = append(result.Items, item)
		}
		if err := rows.Err(); err != nil {
			core.WriteError(w, err)
			return
		}
		if result.HasMore && last > 0 {
			result.Cursor = store.Cursor(scope, filter, fmt.Sprint(last))
		}
		core.WriteJSON(w, http.StatusOK, result)
	}
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readJob(ctx context.Context, db queryer, projectID, id string) (job, error) {
	var item job
	err := db.QueryRowContext(ctx, `SELECT id,project_id,title,brief,COALESCE(worker_id,''),status,revision,created_at,updated_at
		FROM jobs WHERE project_id=? AND id=?`, projectID, id).Scan(&item.ID, &item.ProjectID, &item.Title, &item.Brief,
		&item.WorkerID, &item.Status, &item.Revision, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return job{}, core.Fail("not_found", "job was not found", http.StatusNotFound)
	}
	if err != nil {
		return job{}, err
	}
	return item, nil
}

func appendHistory(ctx context.Context, tx *sql.Tx, scope core.Scope, item job) error {
	p, _, err := readProgress(ctx, tx, item.ProjectID, item.ID, item.Revision)
	if err != nil {
		return err
	}
	state, err := json.Marshal(struct {
		job
		Progress progress `json:"progress"`
	}{item, p})
	if err != nil {
		return core.Fail("internal_error", "could not record job history", http.StatusInternalServerError)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO job_history(project_id,job_id,revision,actor_id,state_json,created_at) VALUES(?,?,?,?,?,?)`,
		item.ProjectID, item.ID, item.Revision, scope.PrincipalID, string(state), item.UpdatedAt)
	if err != nil {
		return err
	}
	return nil
}

func validateContent(title, brief, status string) error {
	if title == "" || len(title) > maxTitle {
		return core.Fail("invalid_title", "title must contain 1 to 240 bytes", http.StatusBadRequest)
	}
	if len(brief) > maxBrief {
		return core.Fail("invalid_brief", "brief exceeds 32768 bytes", http.StatusBadRequest)
	}
	if !validStatuses[status] {
		return core.Fail("invalid_status", "status is not supported", http.StatusBadRequest)
	}
	return nil
}

func validateEdit(req editRequest) error {
	if req.ExpectedRevision < 1 {
		return core.Fail("invalid_revision", "expected_revision must be at least 1", http.StatusBadRequest)
	}
	if req.Title == nil && req.Brief == nil && req.WorkerID == nil && req.Status == nil && req.Progress == nil {
		return core.Fail("empty_edit", "at least one editable field is required", http.StatusBadRequest)
	}
	if req.Title != nil && (*req.Title == "" || len(*req.Title) > maxTitle) {
		return core.Fail("invalid_title", "title must contain 1 to 240 bytes", http.StatusBadRequest)
	}
	if req.Brief != nil && len(*req.Brief) > maxBrief {
		return core.Fail("invalid_brief", "brief exceeds 32768 bytes", http.StatusBadRequest)
	}
	if req.Status != nil && !validStatuses[*req.Status] {
		return core.Fail("invalid_status", "status is not supported", http.StatusBadRequest)
	}
	return nil
}

func revisionConflict(currentRevision int64) error {
	return &core.Error{
		Code:    "revision_conflict",
		Message: "job has changed; reload before editing",
		Status:  http.StatusConflict,
		Details: map[string]int64{"current_revision": currentRevision},
	}
}

func normalizeEdit(req *editRequest) {
	if req.Title != nil {
		value := strings.TrimSpace(*req.Title)
		req.Title = &value
	}
	if req.WorkerID != nil {
		value := strings.TrimSpace(*req.WorkerID)
		req.WorkerID = &value
	}
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
