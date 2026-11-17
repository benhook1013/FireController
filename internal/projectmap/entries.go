package projectmap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

// Register mounts provider-free project-map and job association endpoints.
func Register(mux *http.ServeMux, store *core.Store) {
	registerImport(mux, store)
	base := "/api/v1/projects/{project_id}"
	mux.HandleFunc("POST "+base+"/map/entries", createEntry(store))
	mux.HandleFunc("PUT "+base+"/map/entries/{entry_id}", replaceEntry(store))
	mux.HandleFunc("PATCH "+base+"/map/entries/{entry_id}", updateEntry(store))
	mux.HandleFunc("GET "+base+"/map/entries", listEntries(store))
	mux.HandleFunc("GET "+base+"/map/editorial", editorialEntries(store))
	mux.HandleFunc("GET "+base+"/map/entries/{entry_id}", getEntry(store))
	mux.HandleFunc("GET "+base+"/map/entries/{entry_id}/history", entryHistory(store))
	mux.HandleFunc("GET "+base+"/map/entries/{entry_id}/history/{revision}", getRevision(store))
	mux.HandleFunc("GET "+base+"/jobs/{job_id}/workstream", getJobWorkstream(store))
	mux.HandleFunc("PUT "+base+"/jobs/{job_id}/workstream", replaceJobWorkstream(store))
	mux.HandleFunc("GET "+base+"/jobs/{job_id}/workstream/history", jobWorkstreamHistory(store))
}
func scopeFor(store *core.Store, w http.ResponseWriter, r *http.Request, permission string, keys ...string) (core.Scope, bool) {
	scope, err := store.Scope(r, permission)
	if err == nil {
		_, err = core.QueryKeys(r, "invalid_query", keys...)
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, false
	}
	return scope, true
}
func respond(w http.ResponseWriter, value any, err error, status int) {
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, status, value)
}
func selectedEntry(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("entry_id")
	if !entryID.MatchString(id) {
		core.WriteError(w, invalid())
		return "", false
	}
	return id, true
}
func pageLimit(r *http.Request) (int, error) {
	if _, ok := r.URL.Query()["limit"]; !ok {
		return 25, nil
	}
	return core.PageLimit(r)
}
func revisionCursor(store *core.Store, scope core.Scope, filter string, r *http.Request) (int64, error) {
	value, err := store.ParseCursor(scope, filter, r.URL.Query().Get("cursor"))
	if err != nil {
		return 0, err
	}
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != value {
		return 0, invalid()
	}
	return n, nil
}
func readEntry(ctx context.Context, tx *sql.Tx, project, id string) (Entry, error) {
	var summary Summary
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,kind,title,state,archived,revision,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END FROM project_map_entries WHERE project_id=? AND id=?`, maxEntryBytes, project, id).Scan(&summary.ID, &summary.Kind, &summary.Title, &summary.State, &summary.Archived, &summary.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, notFound()
	}
	if err != nil {
		return Entry{}, err
	}
	if !raw.Valid {
		return Entry{}, unavailable()
	}
	e, err := decodeEntry(raw.String)
	if err != nil {
		return Entry{}, err
	}
	if e.ProjectID != project || e.ID != id || summary != (Summary{e.ID, e.Kind, e.Title, e.State, e.Archived, e.Revision}) {
		return Entry{}, unavailable()
	}
	return e, nil
}
func entryExists(ctx context.Context, tx *sql.Tx, project, id string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM project_map_entries WHERE project_id=? AND id=?`, project, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound()
	}
	return err
}
func writeEntry(ctx context.Context, tx *sql.Tx, scope core.Scope, e Entry, create bool) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if create {
		_, err = tx.ExecContext(ctx, `INSERT INTO project_map_entries(project_id,id,kind,title,state,archived,revision,state_json) VALUES(?,?,?,?,?,?,?,?)`, e.ProjectID, e.ID, e.Kind, e.Title, e.State, e.Archived, e.Revision, string(data))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE project_map_entries SET title=?,state=?,archived=?,revision=?,state_json=? WHERE project_id=? AND id=?`, e.Title, e.State, e.Archived, e.Revision, string(data), e.ProjectID, e.ID)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_map_entry_history(project_id,entry_id,revision,actor_id,state_json,created_at) VALUES(?,?,?,?,?,?)`, e.ProjectID, e.ID, e.Revision, scope.PrincipalID, string(data), e.UpdatedAt)
	if err != nil {
		return err
	}
	kind := "map.entry_updated"
	if create {
		kind = "map.entry_created"
	}
	return core.AppendEvent(ctx, tx, scope, kind, e.ID, e.Revision, map[string]string{"entry_id": e.ID})
}
func createEntry(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		var req editableRequest
		if err := core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		value, err := req.value()
		if err != nil {
			core.WriteError(w, err)
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "map.entries.create", Target: "map.entries", Key: key, Request: value}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			return createEntryTx(ctx, tx, scope, value)
		})
		respond(w, json.RawMessage(result), err, 201)
	}
}
func replaceEntry(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "write")
		if !ok {
			return
		}
		id, ok := selectedEntry(w, r)
		if !ok {
			return
		}
		var raw replaceRequest
		if err := core.ReadJSON(w, r, &raw); err != nil {
			core.WriteError(w, err)
			return
		}
		if !validGuard(raw.ExpectedRevision, 1) || raw.Entry == nil {
			core.WriteError(w, invalid())
			return
		}
		value, err := raw.Entry.value()
		if err != nil {
			core.WriteError(w, err)
			return
		}
		req := replacement{*raw.ExpectedRevision, value}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "map.entries.replace", Target: id, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			return replaceEntryTx(ctx, tx, scope, id, req.ExpectedRevision, value)
		})
		respond(w, json.RawMessage(result), err, 200)
	}
}
func getEntry(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read")
		if !ok {
			return
		}
		id, ok := selectedEntry(w, r)
		if !ok {
			return
		}
		var value Entry
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			value, err = readEntry(ctx, tx, scope.ProjectID, id)
			return err
		})
		respond(w, value, err, 200)
	}
}
func listEntries(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read", "kind", "archived", "cursor", "limit")
		if !ok {
			return
		}
		query := r.URL.Query()
		kind := query.Get("kind")
		if values, present := query["kind"]; present && !validKind(values[0]) {
			core.WriteError(w, invalid())
			return
		}
		archived := false
		if v, present := query["archived"]; present {
			if v[0] != "true" && v[0] != "false" {
				core.WriteError(w, invalid())
				return
			}
			archived = v[0] == "true"
		}
		limit, err := pageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "map.entries:id-asc:" + kind + ":" + strconv.FormatBool(archived)
		after, err := store.ParseCursor(scope, filter, query.Get("cursor"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if after != "" && !entryID.MatchString(after) {
			core.WriteError(w, invalid())
			return
		}
		result := Page[Summary]{Items: make([]Summary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			q := `SELECT id,kind,title,state,archived,revision FROM project_map_entries WHERE project_id=? AND archived=? AND id>?`
			args := []any{scope.ProjectID, archived, after}
			if kind != "" {
				q += ` AND kind=?`
				args = append(args, kind)
			}
			q += ` ORDER BY id LIMIT ?`
			args = append(args, limit+1)
			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item Summary
				if err = rows.Scan(&item.ID, &item.Kind, &item.Title, &item.State, &item.Archived, &item.Revision); err != nil {
					return err
				}
				if !entryID.MatchString(item.ID) || !validKind(item.Kind) || !named(item.Title, 240) || !short(item.State, 240) || item.Revision < 1 {
					return unavailable()
				}
				if len(result.Items) == limit {
					result.HasMore = true
					break
				}
				result.Items = append(result.Items, item)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if result.HasMore {
				result.Cursor = store.Cursor(scope, filter, result.Items[len(result.Items)-1].ID)
			}
			return nil
		})
		respond(w, result, err, 200)
	}
}
func entryHistory(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read", "limit", "cursor")
		if !ok {
			return
		}
		id, ok := selectedEntry(w, r)
		if !ok {
			return
		}
		limit, err := pageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "map.entry-history:" + id + ":revision-desc"
		before, err := revisionCursor(store, scope, filter, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result := Page[HistorySummary]{Items: make([]HistorySummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if err := entryExists(ctx, tx, scope.ProjectID, id); err != nil {
				return err
			}
			query, args := entryHistoryQuery(scope.ProjectID, id, before, limit+1)
			rows, err := tx.QueryContext(ctx, query, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item HistorySummary
				if err = rows.Scan(&item.Revision, &item.ActorID, &item.CreatedAt); err != nil {
					return err
				}
				if item.Revision < 1 || !validID(item.ActorID) || !validStamp(item.CreatedAt) {
					return unavailable()
				}
				if len(result.Items) == limit {
					result.HasMore = true
					break
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
		respond(w, result, err, 200)
	}
}

func entryHistoryQuery(project, id string, before int64, limit int) (string, []any) {
	query := `SELECT revision,actor_id,created_at FROM project_map_entry_history WHERE project_id=? AND entry_id=?`
	args := []any{project, id}
	if before > 0 {
		query += ` AND revision<?`
		args = append(args, before)
	}
	return query + ` ORDER BY revision DESC LIMIT ?`, append(args, limit)
}

// ReadRevision returns one exact retained map revision through the same bounded
// canonical decoder used by the authenticated history endpoint.
func ReadRevision(ctx context.Context, tx *sql.Tx, project, id string, rev int64) (Revision, error) {
	if ctx == nil || tx == nil || !validID(project) || !entryID.MatchString(id) || rev < 1 {
		return Revision{}, invalid()
	}
	var value Revision
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT revision,actor_id,created_at,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END FROM project_map_entry_history WHERE project_id=? AND entry_id=? AND revision=?`, maxEntryBytes, project, id, rev).Scan(&value.Revision, &value.ActorID, &value.CreatedAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, notFound()
	}
	if err != nil {
		return Revision{}, err
	}
	if !raw.Valid {
		return Revision{}, unavailable()
	}
	value.State, err = decodeEntry(raw.String)
	if err != nil {
		return Revision{}, err
	}
	if value.State.ID != id || value.State.ProjectID != project || value.State.Revision != rev || value.State.UpdatedAt != value.CreatedAt || !validID(value.ActorID) {
		return Revision{}, unavailable()
	}
	return value, nil
}

func getRevision(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFor(store, w, r, "read")
		if !ok {
			return
		}
		id, ok := selectedEntry(w, r)
		if !ok {
			return
		}
		rev, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
		if err != nil || rev < 1 || strconv.FormatInt(rev, 10) != r.PathValue("revision") {
			core.WriteError(w, invalid())
			return
		}
		var value Revision
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			value, err = ReadRevision(ctx, tx, scope.ProjectID, id, rev)
			return err
		})
		respond(w, value, err, 200)
	}
}

// These transaction helpers are shared by ordinary mutations and prepared
// imports. Their caller owns authorization, admission, receipt and commit.
func createEntryTx(ctx context.Context, tx *sql.Tx, scope core.Scope, value Editable) (Receipt, error) {
	if err := validateEditable(value); err != nil {
		return Receipt{}, err
	}
	at := stamp()
	e := entry(value, scope.ProjectID, core.NewID("map_entry"), 1, at, at)
	if err := writeEntry(ctx, tx, scope, e, true); err != nil {
		return Receipt{}, err
	}
	return Receipt{e.ID, 1, "created"}, nil
}
func admitEntryReplacement(current Entry, expected int64, value Editable) (bool, error) {
	if !validGuard(&expected, 1) {
		return false, invalid()
	}
	if err := validateEditable(value); err != nil {
		return false, err
	}
	if current.Revision != expected {
		return false, conflict(current.Revision)
	}
	old := editable(current)
	if old.Kind != value.Kind || !retainsPhases(old, value) {
		return false, invalid()
	}
	return !equalEditable(old, value), nil
}
func replaceEntryTx(ctx context.Context, tx *sql.Tx, scope core.Scope, id string, expected int64, value Editable) (Receipt, error) {
	current, err := readEntry(ctx, tx, scope.ProjectID, id)
	if err != nil {
		return Receipt{}, err
	}
	changed, err := admitEntryReplacement(current, expected, value)
	if err != nil {
		return Receipt{}, err
	}
	if !changed {
		return Receipt{id, current.Revision, "unchanged"}, nil
	}
	next := entry(value, scope.ProjectID, id, current.Revision+1, current.CreatedAt, stamp())
	if err := writeEntry(ctx, tx, scope, next, false); err != nil {
		return Receipt{}, err
	}
	return Receipt{id, next.Revision, "updated"}, nil
}
