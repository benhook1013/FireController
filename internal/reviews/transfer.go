package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsite"
)

func previewTransfer(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.AdminScope(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req TransferRequest
		if err = core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		body, err := normalizeTransfer(&req)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var response TransferPreview
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := admitTransfer(ctx, tx, scope.ProjectID, req); err != nil {
				return err
			}
			var err error
			response, err = transferPreview(store.InstanceID(), scope, req, body)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, response)
	}
}

func applyTransfer(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.AdminScope(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req TransferApplyRequest
		if err = core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if len(req.PreviewSHA256) != 64 || strings.Trim(req.PreviewSHA256, "0123456789abcdef") != "" {
			core.WriteError(w, validationError("preview_sha256 must be 64 lowercase hexadecimal characters."))
			return
		}
		body, err := normalizeTransfer(&req.Batch)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		preview, err := transferPreview(store.InstanceID(), scope, req.Batch, body)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if preview.PreviewSHA256 != req.PreviewSHA256 {
			core.WriteError(w, core.Fail("import_preview_mismatch", "The batch or destination differs from the preview; preview the selected batch again.", http.StatusConflict))
			return
		}
		key, err := core.IdempotencyKey(r, "validation_failed")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "reviews.transfer", Target: "workspace", Key: key, Request: req.Batch}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			plan, err := admitTransfer(ctx, tx, scope.ProjectID, req.Batch)
			if err != nil {
				return nil, err
			}
			return publishTransfer(ctx, tx, scope, req.Batch, body, plan)
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, json.RawMessage(result))
	}
}

func publishTransfer(ctx context.Context, tx *sql.Tx, scope core.Scope, req TransferRequest, body []byte, plan transferPlan) (Receipt, error) {
	next := plan.revision + 1
	id := core.NewID("review_snapshot")
	recorded := time.Now().UTC().Format(time.RFC3339Nano)
	observed := plan.observed
	if observed == "" {
		observed = recorded
	}
	digest := sha256.Sum256(body)
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, id, next, req.RepositoryID, req.Publisher, observed, recorded, hex.EncodeToString(digest[:])); err != nil {
		return Receipt{}, err
	}
	write := newHistorySnapshotWrite(scope.ProjectID, next)
	reads := newHistoryReads(tx)
	defer reads.close()
	write.reads = reads
	for _, group := range req.Groups {
		for _, pull := range group.Pulls {
			selected := plan.pulls[pull.Number]
			for _, run := range pull.Runs {
				if err := write.run(ctx, tx, group.Source, pull.Number, selected.runs[runOpaqueID(scope.ProjectID, group.Source, run.SourceRunID)], run); err != nil {
					return Receipt{}, err
				}
			}
			for _, attempt := range pull.Attempts {
				if err := write.attempt(ctx, tx, group.Source, pull.Number, selected.attempts[transferKey{group.Source, attempt.SourceAttemptID}], attempt); err != nil {
					return Receipt{}, err
				}
			}
		}
	}
	totals, err := write.prepareCurrentTotals(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	// Transfer admission forbids owner changes or removals. Only this PR's
	// selected records can affect its compact winners; unrelated staged IDs
	// need not become hundreds of exclusion predicates in every channel query.
	scoped := make(map[int64]map[string]historyCurrentChange, len(plan.order))
	for key, change := range write.pending {
		if change.values == nil || len(change.values) < 2 {
			return Receipt{}, historyInconsistent()
		}
		number, ok := change.values[1].(int64)
		if !ok {
			return Receipt{}, historyInconsistent()
		}
		if scoped[number] == nil {
			scoped[number] = map[string]historyCurrentChange{}
		}
		scoped[number][key] = change
	}
	for _, number := range plan.order {
		selected := plan.pulls[number]
		total, ok := totals[number]
		if !ok {
			total, err = readSummaryTotals(ctx, tx, scope.ProjectID, number)
			if err != nil {
				return Receipt{}, err
			}
		}
		local := *write
		local.pending = scoped[number]
		if local.pending == nil {
			local.pending = map[string]historyCurrentChange{}
		}
		if err := rebuildCurrentSummary(ctx, tx, &local, id, number, selected.position, selected.body, total); err != nil {
			return Receipt{}, err
		}
		for key, change := range local.pending {
			if change.spec.current == historyPullRecord.current {
				write.pending[key] = change
			}
		}
	}
	if err := write.applyCurrent(ctx, tx, id, plan.revision); err != nil {
		return Receipt{}, err
	}
	if err := core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "source": req.Publisher, "imported_history": true, "coverage": "partial"}); err != nil {
		return Receipt{}, err
	}
	if err := publicsite.Refresh(ctx, tx, scope.ProjectID, PublicSiteReviewFacts); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "transferred"}, nil
}
