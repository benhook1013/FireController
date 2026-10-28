package reviews

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewsize"
)

// ActiveQueueResponse is a snapshot-bound status view, not review admission.
type ActiveQueueResponse struct {
	ProjectID  string            `json:"project_id"`
	Configured bool              `json:"configured"`
	SnapshotID string            `json:"snapshot_id"`
	Revision   int64             `json:"revision"`
	Items      []ActiveQueueItem `json:"items"`
	Cursor     string            `json:"cursor,omitempty"`
	HasMore    bool              `json:"has_more"`
}
type ActiveQueueItem struct {
	ReviewSize   *reviewsize.Assessment `json:"review_size,omitempty"`
	Number       int64                  `json:"number"`
	Title        string                 `json:"title"`
	HeadSHA      string                 `json:"head_sha"`
	BaseRef      string                 `json:"base_ref"`
	CIState      string                 `json:"ci_state"`
	Mergeability string                 `json:"mergeability"`
	Position     int                    `json:"position"`
	RunCount     int64                  `json:"run_count"`
}

func activeQueue(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "limit", "cursor")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if len(r.URL.RawQuery) > 4096 {
			core.WriteError(w, validationError("The query exceeds its bounded size."))
			return
		}
		limit, err := core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, present := query["limit"]; !present {
			limit = 5
		}
		const filter = "reviews.queue.active:v1"
		after, err := store.ParseCursor(scope, filter, query.Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		snapshot, position, err := decodePosition(after)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		out := ActiveQueueResponse{ProjectID: scope.ProjectID, Items: make([]ActiveQueueItem, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, snapshot)
			if err != nil {
				return err
			}
			if !found {
				if snapshot != "" {
					return notFound("The review snapshot is unavailable.")
				}
				return nil
			}
			out.Configured, out.SnapshotID, out.Revision = true, meta.SnapshotID, meta.Revision
			// Filter before LIMIT, preserving original queue positions and excluding
			// retired entries without reading run previews, bodies or archives.
			rows, err := tx.QueryContext(ctx, `SELECT p.number,p.title,p.head_sha,p.base_ref,p.ci_state,p.mergeability,p.position,p.run_count FROM review_pulls p WHERE p.project_id=? AND p.snapshot_id=? AND p.delivery_selected=1 AND p.state='open' AND p.position>? AND NOT EXISTS (SELECT 1 FROM review_retired_pulls retired WHERE retired.project_id=p.project_id AND retired.snapshot_id=p.snapshot_id AND retired.number=p.number) ORDER BY p.position LIMIT ?`, scope.ProjectID, meta.SnapshotID, position, limit+1)
			if err != nil {
				return err
			}
			defer rows.Close()
			last := -1
			for rows.Next() {
				var item ActiveQueueItem
				if err := rows.Scan(&item.Number, &item.Title, &item.HeadSHA, &item.BaseRef, &item.CIState, &item.Mergeability, &item.Position, &item.RunCount); err != nil {
					return err
				}
				if len(out.Items) == limit {
					out.HasMore = true
					break
				}
				if item.Number < 1 || item.Number > 2147483647 || !validText(item.Title, 500, true) || !validSHA(item.HeadSHA) || !validText(item.BaseRef, 256, false) || !oneOf(item.CIState, "unknown", "pending", "success", "failure", "not_required") || !oneOf(item.Mergeability, "unknown", "mergeable", "conflicting") || item.Position <= last || item.Position < 0 || item.RunCount < 0 {
					return historyInconsistent()
				}
				out.Items = append(out.Items, item)
				last = item.Position
			}
			if err := rows.Err(); err != nil {
				return err
			}
			for i := range out.Items {
				out.Items[i].ReviewSize = reviewSizeAt(ctx, tx, scope.ProjectID, meta.SnapshotID, out.Items[i].Number, "")
			}
			if out.HasMore {
				out.Cursor = store.Cursor(scope, filter, encodePosition(meta.SnapshotID, last))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, out)
	}
}
