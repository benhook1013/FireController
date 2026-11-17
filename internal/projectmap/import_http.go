package projectmap

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func registerImport(mux *http.ServeMux, store *core.Store) {
	base := "/api/v1/projects/{project_id}/map"
	mux.HandleFunc("POST "+base+"/import/preview", previewPreparedImport(store))
	mux.HandleFunc("POST "+base+"/import", applyImportHTTP(store))
	mux.HandleFunc("GET "+base+"/imports/{source_key}", readImportHTTP(store, false))
	mux.HandleFunc("GET "+base+"/imports/{source_key}/history", importHistoryHTTP(store))
	mux.HandleFunc("GET "+base+"/imports/{source_key}/history/{revision}", readImportHTTP(store, true))
}
func importScope(store *core.Store, w http.ResponseWriter, r *http.Request, write bool, keys ...string) (core.Scope, bool) {
	scope, err := store.AdminScope(r)
	if err == nil {
		_, err = core.QueryKeys(r, "validation_failed", keys...)
	}
	if err == nil && write && scope.WorkerID == "" {
		err = core.Fail("worker_required", "Select an explicit worker for map import.", 400)
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, false
	}
	return scope, true
}
func previewPreparedImport(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := importScope(store, w, r, true)
		if !ok {
			return
		}
		var doc ImportDocument
		if err := core.ReadJSON(w, r, &doc); err != nil {
			core.WriteError(w, err)
			return
		}
		doc, err := NormalizeImportDocument(doc)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		out := ImportPreview{Document: doc}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			out.Plan, out.Conflicts, err = planImport(ctx, tx, scope.ProjectID, doc)
			if err != nil {
				return err
			}
			out.Ready = out.Conflicts == 0
			if out.Ready {
				out.PreviewSHA256, err = importFingerprint(store.InstanceID(), scope, doc)
			}
			return err
		})
		respond(w, out, err, 200)
	}
}
func applyImportHTTP(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := importScope(store, w, r, true)
		if !ok {
			return
		}
		var req ImportApplyRequest
		if err := core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if len(req.PreviewSHA256) != 64 || strings.Trim(req.PreviewSHA256, "0123456789abcdef") != "" {
			core.WriteError(w, invalid())
			return
		}
		doc, err := NormalizeImportDocument(req.Document)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		fingerprint, err := importFingerprint(store.InstanceID(), scope, doc)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if fingerprint != req.PreviewSHA256 {
			core.WriteError(w, core.Fail("import_preview_mismatch", "The document or selected destination changed; preview again.", 409))
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := applyPreparedImport(r.Context(), store, scope, key, doc)
		respond(w, json.RawMessage(result), err, 200)
	}
}
func importSelector(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("source_key")
	if !importKey.MatchString(key) {
		core.WriteError(w, invalid())
		return "", false
	}
	return key, true
}
func readImportHTTP(store *core.Store, historical bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := importScope(store, w, r, false)
		if !ok {
			return
		}
		source, ok := importSelector(w, r)
		if !ok {
			return
		}
		revision := int64(0)
		if historical {
			var err error
			revision, err = strconv.ParseInt(r.PathValue("revision"), 10, 64)
			if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != r.PathValue("revision") {
				core.WriteError(w, invalid())
				return
			}
		}
		var out ImportRecord
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			out, err = readImportRecord(ctx, tx, scope.ProjectID, source, revision)
			return err
		})
		respond(w, out, err, 200)
	}
}
func importHistoryHTTP(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := importScope(store, w, r, false, "limit", "cursor")
		if !ok {
			return
		}
		source, ok := importSelector(w, r)
		if !ok {
			return
		}
		limit, err := pageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "map.import-history:" + source + ":revision-desc"
		before, err := revisionCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		out := Page[ImportHistorySummary]{Items: make([]ImportHistorySummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM project_map_import_sources WHERE project_id=? AND source_key=?`, scope.ProjectID, source).Scan(&exists); err == sql.ErrNoRows {
				return notFound()
			} else if err != nil {
				return err
			}
			query := `SELECT revision,actor_id,worker_id,created_at FROM project_map_import_history WHERE project_id=? AND source_key=?`
			args := []any{scope.ProjectID, source}
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
				var item ImportHistorySummary
				if err := rows.Scan(&item.Revision, &item.ActorID, &item.WorkerID, &item.CreatedAt); err != nil {
					return err
				}
				if item.Revision < 1 || !validID(item.ActorID) || !validID(item.WorkerID) || !validStamp(item.CreatedAt) {
					return unavailable()
				}
				if len(out.Items) == limit {
					out.HasMore = true
					break
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
		respond(w, out, err, 200)
	}
}
