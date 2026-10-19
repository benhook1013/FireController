package reviews

// Frozen predecessor queue/pull readers are test-only wire oracles for genuine
// old schemas. Production accepts requests only after all startup migrations.
import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"testing"
)

func metadataFixtureReaders(store *core.Store, fallback http.Handler) http.Handler {
	legacy := http.NewServeMux()
	legacy.HandleFunc("GET /api/v1/projects/{project_id}/reviews/queue", metadataPre7Queue(store))
	legacy.HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{number}", metadataPre7Pull(store))
	legacy.Handle("/", fallback)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var version int
		if store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version) == nil && version < 7 {
			legacy.ServeHTTP(w, r)
		} else {
			fallback.ServeHTTP(w, r)
		}
	})
}
func removeMetadataForPre7Fixture(t *testing.T, store *core.Store) {
	t.Helper()
	for _, statement := range []string{`DROP VIEW review_pulls`, `ALTER TABLE review_pull_versions DROP COLUMN metadata_json`, HistoryReadViewSQLV6()["review_pulls"]} {
		if _, err := store.DB().Exec(statement); err != nil {
			t.Fatal("genuine predecessor metadata fixture", err)
		}
	}
}
func metadataPre7Queue(store *core.Store) http.HandlerFunc {
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
		filter := "reviews.queue:v1"
		after, err := store.ParseCursor(scope, filter, query.Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		snapshotID, position, err := decodePosition(after)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		response := QueueResponse{ProjectID: scope.ProjectID, Items: make([]PullSummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, snapshotID)
			if err != nil {
				return err
			}
			if !found {
				if snapshotID != "" {
					return notFound("The review snapshot is unavailable.")
				}
				return nil
			}
			response.Configured = true
			setMetaQueue(&response, meta)
			rows, err := tx.QueryContext(ctx, `SELECT p.number,p.title,p.state,p.head_sha,p.base_ref,p.base_sha,p.ci_state,p.mergeability,p.complete,p.run_count,p.attempt_count,p.recent_runs_json,p.latest_attempts_json,p.position
				FROM review_pulls p WHERE p.project_id=? AND p.snapshot_id=? AND p.position>? AND NOT EXISTS (
					SELECT 1 FROM review_retired_pulls retired WHERE retired.project_id=p.project_id AND retired.snapshot_id=p.snapshot_id AND retired.number=p.number
				) ORDER BY p.position LIMIT ?`,
				scope.ProjectID, meta.SnapshotID, position, limit+1)
			if err != nil {
				return err
			}
			defer rows.Close()
			lastPosition := -1
			for rows.Next() {
				var item PullSummary
				var complete int
				var recentJSON, latestJSON string
				var rowPosition int
				if err := rows.Scan(&item.Number, &item.Title, &item.State, &item.HeadSHA, &item.BaseRef, &item.BaseSHA, &item.CIState, &item.Mergeability, &complete, &item.RunCount, &item.AttemptCount, &recentJSON, &latestJSON, &rowPosition); err != nil {
					return err
				}
				if len(response.Items) == limit {
					response.HasMore = true
					break
				}
				item.Complete = complete != 0
				if err := strictStoredJSON(recentJSON, &item.RecentRuns); err != nil {
					return err
				}
				if err := fillRecentRunSources(ctx, tx, scope.ProjectID, meta.SnapshotID, item.Number, item.RecentRuns); err != nil {
					return err
				}
				if err := strictStoredJSON(latestJSON, &item.LatestAttempts); err != nil {
					return err
				}
				response.Items = append(response.Items, item)
				lastPosition = rowPosition
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

func metadataPre7Pull(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.Scope(r, "read")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		query, err := core.QueryKeys(r, "validation_failed", "snapshot")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if len(r.URL.RawQuery) > 4096 {
			core.WriteError(w, validationError("The query exceeds its bounded size."))
			return
		}
		number, err := parsePullNumber(r.PathValue("number"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		response := PullResponse{ProjectID: scope.ProjectID}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			meta, found, err := selectSnapshot(ctx, tx, scope.ProjectID, query.Get("snapshot"))
			if err != nil {
				return err
			}
			if !found {
				return notFound("The review snapshot is unavailable.")
			}
			setMetaPull(&response, meta)
			var complete int
			var recentJSON, latestJSON string
			err = tx.QueryRowContext(ctx, `SELECT number,title,state,head_sha,base_ref,base_sha,ci_state,mergeability,complete,run_count,attempt_count,recent_runs_json,latest_attempts_json FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, scope.ProjectID, meta.SnapshotID, number).Scan(
				&response.Pull.Number, &response.Pull.Title, &response.Pull.State, &response.Pull.HeadSHA, &response.Pull.BaseRef, &response.Pull.BaseSHA, &response.Pull.CIState, &response.Pull.Mergeability, &complete, &response.Pull.RunCount, &response.Pull.AttemptCount, &recentJSON, &latestJSON)
			if errors.Is(err, sql.ErrNoRows) {
				return notFound("The pull request is unavailable in this snapshot.")
			}
			if err != nil {
				return err
			}
			response.Pull.Complete = complete != 0
			if err := strictStoredJSON(recentJSON, &response.Pull.RecentRuns); err != nil {
				return err
			}
			if err := fillRecentRunSources(ctx, tx, scope.ProjectID, meta.SnapshotID, number, response.Pull.RecentRuns); err != nil {
				return err
			}
			if err := strictStoredJSON(latestJSON, &response.Pull.LatestAttempts); err != nil {
				return err
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
