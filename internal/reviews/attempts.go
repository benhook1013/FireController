package reviews

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

// attempts reads one immutable, newest-first archive page. Producer identity is
// included because an external attempt ID is not unique across producers.
func attempts(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "snapshot", "limit", "cursor")
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
		number, err := parsePullNumber(r.PathValue("number"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := fmt.Sprintf("reviews.attempts:%d:v1", number)
		after, err := store.ParseCursor(scope, filter, query.Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		cursorSnapshot, cursorPosition, err := decodePosition(after)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		requested := query.Get("snapshot")
		if requested != "" && cursorSnapshot != "" && requested != cursorSnapshot {
			core.WriteError(w, validationError("The snapshot must match the attempts cursor."))
			return
		}
		if requested == "" {
			requested = cursorSnapshot
		}
		response := AttemptsResponse{ProjectID: scope.ProjectID, Items: make([]AttemptHistoryItem, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, requested)
			if err != nil {
				return err
			}
			if !found {
				return notFound("The review snapshot is unavailable.")
			}
			response.Revision, response.SnapshotID, response.Source = meta.Revision, meta.SnapshotID, meta.Source
			response.ObservedAt, response.RecordedAt = meta.ObservedAt, meta.RecordedAt
			if _, err := pullExists(ctx, tx, scope.ProjectID, meta.SnapshotID, number); err != nil {
				return err
			}
			querySQL := `SELECT source,source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id,reason,position FROM review_attempts WHERE project_id=? AND snapshot_id=? AND pull_number=?`
			args := []any{scope.ProjectID, meta.SnapshotID, number}
			if cursorSnapshot != "" {
				querySQL += ` AND position<?`
				args = append(args, cursorPosition)
			}
			querySQL += ` ORDER BY position DESC LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, querySQL, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			lastPosition := -1
			for rows.Next() {
				var item AttemptHistoryItem
				var position int
				if err := rows.Scan(&item.Source, &item.SourceAttemptID, &item.Channel, &item.HeadSHA, &item.State, &item.StartedAt, &item.FinishedAt, &item.DurationSeconds, &item.SourceRunID, &item.TriggerID, &item.ProviderReviewID, &item.Reason, &position); err != nil {
					return err
				}
				if !ValidAttemptReason(item.State, item.Reason) {
					return historyInconsistent()
				}
				if len(response.Items) == limit {
					response.HasMore = true
					break
				}
				response.Items = append(response.Items, item)
				lastPosition = position
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if response.HasMore && lastPosition >= 0 {
				response.Cursor = store.Cursor(scope, filter, encodePosition(meta.SnapshotID, lastPosition))
			}
			return nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, response)
	}
}
