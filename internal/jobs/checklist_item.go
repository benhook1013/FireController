package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

type checklistItemPatch struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	Done             *bool  `json:"done"`
}

func updateChecklistItem(store *core.Store) http.HandlerFunc {
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
		var req checklistItemPatch
		if err = core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if req.ExpectedRevision == nil || *req.ExpectedRevision < 1 || *req.ExpectedRevision == math.MaxInt64 || req.Done == nil || !checklistItemID.MatchString(r.PathValue("item_id")) {
			core.WriteError(w, core.Fail("validation_failed", "Supply a writable checklist revision, existing item ID and explicit boolean done.", 400))
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		jobID, itemID := r.PathValue("job_id"), r.PathValue("item_id")
		request := struct {
			ItemID string `json:"item_id"`
			checklistItemPatch
		}{itemID, req}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "jobs.checklist_item", Target: jobID, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			current, err := readJob(ctx, tx, scope.ProjectID, jobID)
			if err != nil {
				return nil, err
			}
			revision, items, err := readChecklist(ctx, tx, scope.ProjectID, jobID)
			if err != nil {
				return nil, err
			}
			if revision != *req.ExpectedRevision {
				return nil, &core.Error{Code: "checklist_revision_conflict", Message: "Reload the current checklist before changing an item.", Status: 409, Details: map[string]any{"current_revision": revision}}
			}
			selected := -1
			for i := range items {
				if items[i].ID == itemID {
					selected = i
					break
				}
			}
			if selected < 0 {
				return nil, core.Fail("not_found", "Checklist item was not found.", 404)
			}
			receipt := checklistReceipt{ID: jobID, Revision: revision, JobRevision: current.Revision, Outcome: "unchanged"}
			if items[selected].Done == *req.Done {
				return receipt, nil
			}
			items[selected].Done = *req.Done
			body, err := json.Marshal(items)
			if err != nil {
				return nil, err
			}
			revision++
			at := timestamp()
			_, err = tx.ExecContext(ctx, `UPDATE job_checklists SET revision=?,job_revision=?,actor_id=?,worker_id=?,items_json=?,created_at=? WHERE project_id=? AND job_id=?`, revision, current.Revision, scope.PrincipalID, scope.WorkerID, string(body), at, scope.ProjectID, jobID)
			if err != nil {
				return nil, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO job_checklist_history(project_id,job_id,revision,job_revision,actor_id,worker_id,items_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, jobID, revision, current.Revision, scope.PrincipalID, scope.WorkerID, string(body), at)
			if err != nil {
				return nil, err
			}
			// Reuse the checklist-state notification consumed by project replay
			// and browser invalidation, including item-only changes.
			if err = core.AppendEvent(ctx, tx, scope, "job.checklist_replaced", jobID, revision, map[string]any{"job_id": jobID, "item_id": itemID, "checklist_revision": revision}); err != nil {
				return nil, err
			}
			receipt.Revision, receipt.Outcome = revision, "updated"
			return receipt, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, json.RawMessage(result))
	}
}
