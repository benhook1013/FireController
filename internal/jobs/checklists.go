package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

var checklistItemID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type checklistItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}
type checklistItemRequest struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done *bool  `json:"done"`
}
type checklistRequest struct {
	ExpectedJobRevision *int64                 `json:"expected_job_revision"`
	ExpectedRevision    *int64                 `json:"expected_revision"`
	Items               []checklistItemRequest `json:"items"`
}
type checklistReplacement struct {
	ExpectedJobRevision int64           `json:"expected_job_revision"`
	ExpectedRevision    int64           `json:"expected_revision"`
	Items               []checklistItem `json:"items"`
}
type checklistReceipt struct {
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	JobRevision int64  `json:"job_revision"`
	Outcome     string `json:"outcome"`
}
type checklistResponse struct {
	JobID       string          `json:"job_id"`
	JobRevision int64           `json:"job_revision"`
	Revision    int64           `json:"revision"`
	Items       []checklistItem `json:"items"`
}
type checklistEntry struct {
	Revision    int64           `json:"revision"`
	JobRevision int64           `json:"job_revision"`
	ActorID     string          `json:"actor_id"`
	WorkerID    string          `json:"worker_id"`
	Items       []checklistItem `json:"items"`
	CreatedAt   string          `json:"created_at"`
}
type checklistPage struct {
	Items   []checklistEntry `json:"items"`
	Cursor  string           `json:"cursor,omitempty"`
	HasMore bool             `json:"has_more"`
}

func validChecklistItems(items []checklistItem) bool {
	if items == nil || len(items) > 100 {
		return false
	}
	seen := map[string]bool{}
	size := 0
	for _, item := range items {
		if !checklistItemID.MatchString(item.ID) || seen[item.ID] || !validCheckpointText(item.Text, 1024, true) {
			return false
		}
		seen[item.ID] = true
		size += len(item.ID) + len(item.Text)
	}
	return size <= 16<<10
}
func decodeChecklistItems(raw string) ([]checklistItem, error) {
	var items []checklistItem
	if strictjson.Decode([]byte(raw), &items) != nil || !validChecklistItems(items) {
		return nil, core.Fail("storage_unavailable", "Stored checklist is unavailable.", http.StatusInternalServerError)
	}
	canonical, err := json.Marshal(items)
	if err != nil || string(canonical) != raw {
		return nil, core.Fail("storage_unavailable", "Stored checklist is unavailable.", http.StatusInternalServerError)
	}
	return items, nil
}
func readChecklist(ctx context.Context, tx *sql.Tx, project, job string) (int64, []checklistItem, error) {
	var revision int64
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT revision,items_json FROM job_checklists WHERE project_id=? AND job_id=?`, project, job).Scan(&revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, []checklistItem{}, nil
	}
	if err != nil {
		return 0, nil, err
	}
	items, err := decodeChecklistItems(raw)
	if err != nil || revision < 1 {
		return 0, nil, core.Fail("storage_unavailable", "Stored checklist is unavailable.", http.StatusInternalServerError)
	}
	return revision, items, nil
}
func getChecklist(store *core.Store) http.HandlerFunc {
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
		result := checklistResponse{JobID: r.PathValue("job_id")}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readJob(ctx, tx, scope.ProjectID, result.JobID)
			if err != nil {
				return err
			}
			result.JobRevision = current.Revision
			result.Revision, result.Items, err = readChecklist(ctx, tx, scope.ProjectID, result.JobID)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		writeProgressJSON(w, result, maxCurrentChecklist)
	}
}
func replaceChecklist(store *core.Store) http.HandlerFunc {
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
		var raw checklistRequest
		if err = core.ReadJSON(w, r, &raw); err != nil {
			core.WriteError(w, err)
			return
		}
		if raw.ExpectedJobRevision == nil || *raw.ExpectedJobRevision < 1 || raw.ExpectedRevision == nil || *raw.ExpectedRevision < 0 || *raw.ExpectedRevision == math.MaxInt64 {
			core.WriteError(w, core.Fail("invalid_revision", "Both current revisions are required; expected_revision must allow a next revision.", 400))
			return
		}
		req := checklistReplacement{ExpectedJobRevision: *raw.ExpectedJobRevision, ExpectedRevision: *raw.ExpectedRevision, Items: make([]checklistItem, 0, len(raw.Items))}
		if raw.Items == nil {
			core.WriteError(w, core.Fail("validation_failed", "items must be an explicit array.", 400))
			return
		}
		for _, item := range raw.Items {
			if item.Done == nil {
				core.WriteError(w, core.Fail("validation_failed", "Each checklist item requires an explicit boolean done.", 400))
				return
			}
			req.Items = append(req.Items, checklistItem{ID: item.ID, Text: item.Text, Done: *item.Done})
		}
		if !validChecklistItems(req.Items) {
			core.WriteError(w, core.Fail("validation_failed", "Use at most 100 unique checklist items and 16384 bytes of valid item content.", 400))
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		jobID := r.PathValue("job_id")
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "jobs.checklist", Target: jobID, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := readJob(ctx, tx, scope.ProjectID, jobID)
			if err != nil {
				return nil, err
			}
			if current.Revision != req.ExpectedJobRevision {
				return nil, revisionConflict(current.Revision)
			}
			revision, items, err := readChecklist(ctx, tx, scope.ProjectID, jobID)
			if err != nil {
				return nil, err
			}
			if revision != req.ExpectedRevision {
				return nil, &core.Error{Code: "checklist_revision_conflict", Message: "Reload the current checklist before replacing it.", Status: 409, Details: map[string]any{"current_revision": revision}}
			}
			body, err := json.Marshal(req.Items)
			if err != nil {
				return nil, err
			}
			old, err := json.Marshal(items)
			if err != nil {
				return nil, err
			}
			receipt := checklistReceipt{ID: jobID, Revision: revision, JobRevision: current.Revision, Outcome: "unchanged"}
			if string(old) == string(body) {
				return receipt, nil
			}
			revision++
			at := timestamp()
			_, err = tx.ExecContext(ctx, `INSERT INTO job_checklists(project_id,job_id,revision,job_revision,actor_id,worker_id,items_json,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(project_id,job_id) DO UPDATE SET revision=excluded.revision,job_revision=excluded.job_revision,actor_id=excluded.actor_id,worker_id=excluded.worker_id,items_json=excluded.items_json,created_at=excluded.created_at`, scope.ProjectID, jobID, revision, current.Revision, scope.PrincipalID, scope.WorkerID, string(body), at)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO job_checklist_history(project_id,job_id,revision,job_revision,actor_id,worker_id,items_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, jobID, revision, current.Revision, scope.PrincipalID, scope.WorkerID, string(body), at)
			if err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "job.checklist_replaced", jobID, revision, map[string]any{"job_id": jobID, "checklist_revision": revision}); err != nil {
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
func checklistHistory(store *core.Store) http.HandlerFunc {
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
		filter := "job-checklist:" + scope.ProjectID + ":" + jobID + ":revision-desc"
		before, err := progressCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := checklistPage{Items: make([]checklistEntry, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := readJob(ctx, tx, scope.ProjectID, jobID); err != nil {
				return err
			}
			query := `SELECT revision,job_revision,actor_id,worker_id,items_json,created_at FROM job_checklist_history WHERE project_id=? AND job_id=?`
			args := []any{scope.ProjectID, jobID}
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
				var item checklistEntry
				var raw string
				if err = rows.Scan(&item.Revision, &item.JobRevision, &item.ActorID, &item.WorkerID, &raw, &item.CreatedAt); err != nil {
					return err
				}
				item.Items, err = decodeChecklistItems(raw)
				if err != nil {
					return err
				}
				result.Items = append(result.Items, item)
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
