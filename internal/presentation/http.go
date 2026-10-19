package presentation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/presentationmodel"
)

func Register(mux *http.ServeMux, store *core.Store) {
	base := "/api/v1/projects/{project_id}/presentation"
	mux.HandleFunc("GET "+base, current(store))
	mux.HandleFunc("PUT "+base, replace(store))
	mux.HandleFunc("GET "+base+"/history", history(store))
	mux.HandleFunc("GET "+base+"/history/{revision}", revision(store))
}
func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, v)
}
func readScope(store *core.Store, w http.ResponseWriter, r *http.Request, keys ...string) (core.Scope, bool) {
	scope, err := store.Scope(r, "read")
	if err == nil {
		_, err = core.QueryKeys(r, "invalid_query", keys...)
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, false
	}
	return scope, true
}
func readCurrent(ctx context.Context, tx *sql.Tx, project string) (State, error) {
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=?`, project).Scan(&name); err != nil {
		return State{}, err
	}
	var rev int64
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT revision,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END FROM project_presentations WHERE project_id=?`, maxStateBytes, project).Scan(&rev, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		// Preserve a retained name verbatim, even if it predates the current
		// name contract. Reads do not repair it; an administrator can replace
		// revision zero with a validated configuration atomically.
		defaults := presentationmodel.Defaults()
		c := Config{Name: name, BrandName: defaults.BrandName, Tagline: defaults.Tagline, Headings: map[string]string{}, Theme: map[string]string{}}
		return state(c, project, 0, ""), nil
	}
	if err != nil {
		return State{}, err
	}
	s, err := decodeState(raw)
	if err != nil {
		return State{}, err
	}
	if s.ProjectID != project || s.Revision != rev || s.Name != name {
		return State{}, unavailable()
	}
	return s, nil
}
func current(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := readScope(store, w, r)
		if !ok {
			return
		}
		var out Snapshot
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			out.Presentation, err = readCurrent(ctx, tx, scope.ProjectID)
			if err != nil {
				return err
			}
			out.EventCursor, err = store.ProjectEventCursor(ctx, tx, scope)
			return err
		})
		respond(w, out, err)
	}
}

type replaceBody struct {
	ExpectedRevision *int64          `json:"expected_revision"`
	Presentation     json.RawMessage `json:"presentation"`
}
type replacement struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Presentation     Config `json:"presentation"`
}

func replace(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.AdminScope(r)
		if err == nil {
			_, err = core.QueryKeys(r, "invalid_query")
		}
		if err == nil && scope.WorkerID == "" {
			err = core.Fail("worker_required", "Bind an explicit worker before replacing project presentation.", 400)
		}
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var body replaceBody
		if err = core.ReadJSON(w, r, &body); err != nil {
			core.WriteError(w, err)
			return
		}
		if body.ExpectedRevision == nil || *body.ExpectedRevision < 0 || *body.ExpectedRevision == math.MaxInt64 {
			core.WriteError(w, invalid())
			return
		}
		if len(body.Presentation) == 0 {
			core.WriteError(w, invalid())
			return
		}
		c, err := presentationmodel.Decode(body.Presentation)
		if err != nil {
			core.WriteError(w, invalid())
			return
		}
		key, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		req := replacement{*body.ExpectedRevision, c}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "project.presentation.replace", Target: scope.ProjectID, Key: key, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			old, err := readCurrent(ctx, tx, scope.ProjectID)
			if err != nil {
				return nil, err
			}
			if old.Revision != req.ExpectedRevision {
				return nil, conflict(old.Revision)
			}
			if equalConfig(old.Config(), c) {
				return Receipt{scope.ProjectID, old.Revision, "unchanged"}, nil
			}
			next := state(c, scope.ProjectID, old.Revision+1, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
			raw, err := json.Marshal(next)
			if err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO project_presentations(project_id,revision,state_json) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET revision=excluded.revision,state_json=excluded.state_json`, scope.ProjectID, next.Revision, string(raw)); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO project_presentation_history(project_id,revision,actor_id,state_json,created_at) VALUES(?,?,?,?,?)`, scope.ProjectID, next.Revision, scope.PrincipalID, string(raw), next.UpdatedAt); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE projects SET name=? WHERE id=?`, c.Name, scope.ProjectID); err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "project.presentation_changed", scope.ProjectID, next.Revision, map[string]string{"project_id": scope.ProjectID}); err != nil {
				return nil, err
			}
			return Receipt{scope.ProjectID, next.Revision, "updated"}, nil
		})
		respond(w, json.RawMessage(result), err)
	}
}
func selectedRevision(v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != v {
		return 0, invalid()
	}
	return n, nil
}
func readRevision(ctx context.Context, tx *sql.Tx, project string, n int64) (Revision, error) {
	var out Revision
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT revision,actor_id,created_at,CASE WHEN length(CAST(state_json AS BLOB))<=? THEN state_json ELSE NULL END FROM project_presentation_history WHERE project_id=? AND revision=?`, maxStateBytes, project, n).Scan(&out.Revision, &out.ActorID, &out.CreatedAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, notFound()
	}
	if err != nil {
		return Revision{}, err
	}
	out.State, err = decodeState(raw)
	if err != nil {
		return Revision{}, err
	}
	if out.Revision != n || out.State.Revision != n || out.State.ProjectID != project || out.CreatedAt != out.State.UpdatedAt || !validID(out.ActorID) {
		return Revision{}, unavailable()
	}
	return out, nil
}
func revision(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := readScope(store, w, r)
		if !ok {
			return
		}
		n, err := selectedRevision(r.PathValue("revision"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var out Revision
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var e error
			out, e = readRevision(ctx, tx, scope.ProjectID, n)
			return e
		})
		respond(w, out, err)
	}
}
func historyQuery(project string, before int64, limit int) (string, []any) {
	q := `SELECT revision,actor_id,created_at FROM project_presentation_history WHERE project_id=?`
	args := []any{project}
	if before > 0 {
		q += ` AND revision<?`
		args = append(args, before)
	}
	return q + ` ORDER BY revision DESC LIMIT ?`, append(args, limit)
}
func history(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := readScope(store, w, r, "limit", "cursor")
		if !ok {
			return
		}
		limit := 25
		var err error
		if _, ok := r.URL.Query()["limit"]; ok {
			limit, err = core.PageLimit(r)
		}
		if err != nil {
			core.WriteError(w, err)
			return
		}
		const filter = "project.presentation-history:revision-desc"
		after, err := store.ParseCursor(scope, filter, r.URL.Query().Get("cursor"))
		var before int64
		if err == nil && after != "" {
			before, err = selectedRevision(after)
		}
		if err != nil {
			core.WriteError(w, err)
			return
		}
		out := Page{Items: make([]HistorySummary, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			q, args := historyQuery(scope.ProjectID, before, limit+1)
			rows, err := tx.QueryContext(ctx, q, args...)
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
				if len(out.Items) == limit {
					out.HasMore = true
					break
				}
				out.Items = append(out.Items, item)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if out.HasMore {
				out.Cursor = store.Cursor(scope, filter, strconv.FormatInt(out.Items[len(out.Items)-1].Revision, 10))
			}
			return nil
		})
		respond(w, out, err)
	}
}
