// Package opvars owns operator variables: short opaque key/value strings set
// by an administrator and read by any project credential. The service stores,
// resolves and returns them; it never interprets a value.
package opvars

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/opvarsmodel"
)

// EnsureSchema creates the single append-only history table. The current
// value of a variable is its highest retained revision; a NULL value records
// an unset. Instance-wide rows use the empty project identity.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "operator_variables", []string{
		`CREATE TABLE operator_variable_history (
 project_id TEXT NOT NULL,
 key TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 value TEXT,
 actor_id TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(project_id,key,revision)
 );`,
	})
}

// Variable is one effective value after resolving a project override over the
// instance value.
type Variable struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Scope     string `json:"scope"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

// List is the complete effective variable set visible to one project.
type List struct {
	Variables []Variable `json:"variables"`
}

// Entry is one scope's current state for a key. Set is false after an unset,
// and Value is then omitted.
type Entry struct {
	Set       bool   `json:"set"`
	Value     string `json:"value,omitempty"`
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	UpdatedAt string `json:"updated_at"`
}

// Detail reports the effective value of one key and both scopes behind it. A
// scope that has never written the key is omitted.
type Detail struct {
	Key      string `json:"key"`
	Set      bool   `json:"set"`
	Value    string `json:"value,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Instance *Entry `json:"instance,omitempty"`
	Project  *Entry `json:"project,omitempty"`
}

// Receipt reports one write.
type Receipt struct {
	Key      string `json:"key"`
	Scope    string `json:"scope"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

// HistoryItem is one retained revision of a key in one scope; Set is false
// for an unset revision.
type HistoryItem struct {
	Revision  int64  `json:"revision"`
	Set       bool   `json:"set"`
	Value     string `json:"value,omitempty"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
}

// Page is one newest-first history page.
type Page struct {
	Items   []HistoryItem `json:"items"`
	Cursor  string        `json:"cursor,omitempty"`
	HasMore bool          `json:"has_more"`
}

func invalid() error {
	return core.Fail("validation_failed", "Use a lowercase variable key of at most 64 bytes, scope instance or project, and a value of 1-240 UTF-8 bytes without control characters or surrounding whitespace (null unsets).", 400)
}
func unavailable() error {
	return core.Fail("storage_unavailable", "Stored operator variables are unavailable.", 503)
}
func conflict(rev int64) error {
	return &core.Error{Code: "operator_variable_revision_conflict", Message: "Reload the variable before replacing it.", Status: 409, Details: map[string]int64{"current_revision": rev}}
}
func tooMany() error {
	return core.Fail("operator_variable_limit", "A scope holds at most 32 set variables; unset one first.", 409)
}
func instanceAdminRequired() error {
	return core.Fail("forbidden", "Instance-wide variables require an instance administrator credential.", 403)
}

func validActor(s string) bool {
	return s != "" && len(s) <= 128 && utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0
}
func validStamp(v string) bool {
	t, e := time.Parse(time.RFC3339Nano, v)
	return e == nil && !t.IsZero() && strings.HasSuffix(v, "Z")
}

func storageProject(scope, project string) string {
	if scope == opvarsmodel.ScopeInstance {
		return ""
	}
	return project
}

// Register mounts the project-scoped routes. Reads use ordinary project read
// permission; writes use administrative scope, and instance-wide writes also
// require an instance administrator.
func Register(mux *http.ServeMux, store *core.Store) {
	base := "/api/v1/projects/{project_id}/vars"
	mux.HandleFunc("GET "+base, list(store))
	mux.HandleFunc("GET "+base+"/{key}", read(store))
	mux.HandleFunc("PUT "+base+"/{key}", write(store))
	mux.HandleFunc("GET "+base+"/{key}/history", history(store))
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, v)
}

func readScope(store *core.Store, w http.ResponseWriter, r *http.Request, keys ...string) (core.Scope, queryValues, bool) {
	scope, err := store.Scope(r, "read")
	var q queryValues
	if err == nil {
		q.values, err = core.QueryKeys(r, "invalid_query", keys...)
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, q, false
	}
	return scope, q, true
}

type queryValues struct{ values map[string][]string }

func (u queryValues) get(k string) (string, bool) {
	v, ok := u.values[k]
	if !ok || len(v) != 1 {
		return "", false
	}
	return v[0], true
}

func scanEntry(value sql.NullString, rev int64, actor, at string) (Entry, error) {
	e := Entry{Revision: rev, ActorID: actor, UpdatedAt: at}
	if value.Valid {
		if !opvarsmodel.ValidValue(value.String) {
			return Entry{}, unavailable()
		}
		e.Set, e.Value = true, value.String
	}
	if rev < 1 || !validActor(actor) || !validStamp(at) {
		return Entry{}, unavailable()
	}
	return e, nil
}

const currentQuery = `SELECT h.project_id,h.key,h.revision,h.value,h.actor_id,h.created_at FROM operator_variable_history h
 WHERE h.project_id IN ('',?) AND h.revision=(SELECT max(revision) FROM operator_variable_history m WHERE m.project_id=h.project_id AND m.key=h.key)`

// current returns the latest entry per key for the instance and the project.
func current(ctx context.Context, tx *sql.Tx, project, key string) (instance, overrides map[string]Entry, err error) {
	q, args := currentQuery, []any{project}
	if key != "" {
		q += ` AND h.key=?`
		args = append(args, key)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY h.key,h.project_id`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	instance, overrides = map[string]Entry{}, map[string]Entry{}
	for rows.Next() {
		var p, k, actor, at string
		var rev int64
		var value sql.NullString
		if err = rows.Scan(&p, &k, &rev, &value, &actor, &at); err != nil {
			return nil, nil, err
		}
		if !opvarsmodel.ValidKey(k) {
			return nil, nil, unavailable()
		}
		e, err := scanEntry(value, rev, actor, at)
		if err != nil {
			return nil, nil, err
		}
		if p == "" {
			instance[k] = e
		} else {
			overrides[k] = e
		}
	}
	return instance, overrides, rows.Err()
}

// Effective resolves the complete variable set for a project inside the
// caller's read transaction.
func Effective(ctx context.Context, tx *sql.Tx, project string) ([]Variable, error) {
	instance, overrides, err := current(ctx, tx, project, "")
	if err != nil {
		return nil, err
	}
	out := make([]Variable, 0, len(instance)+len(overrides))
	pick := func(k string) {
		if e, ok := overrides[k]; ok && e.Set {
			out = append(out, Variable{k, e.Value, opvarsmodel.ScopeProject, e.Revision, e.UpdatedAt})
		} else if e, ok := instance[k]; ok && e.Set {
			out = append(out, Variable{k, e.Value, opvarsmodel.ScopeInstance, e.Revision, e.UpdatedAt})
		}
	}
	seen := map[string]bool{}
	for _, m := range []map[string]Entry{overrides, instance} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				pick(k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func list(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, _, ok := readScope(store, w, r)
		if !ok {
			return
		}
		var out List
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			out.Variables, err = Effective(ctx, tx, scope.ProjectID)
			return err
		})
		respond(w, out, err)
	}
}

func readDetail(ctx context.Context, tx *sql.Tx, project, key string) (Detail, error) {
	instance, overrides, err := current(ctx, tx, project, key)
	if err != nil {
		return Detail{}, err
	}
	out := Detail{Key: key}
	if e, ok := instance[key]; ok {
		out.Instance = &e
		if e.Set {
			out.Set, out.Value, out.Scope = true, e.Value, opvarsmodel.ScopeInstance
		}
	}
	if e, ok := overrides[key]; ok {
		out.Project = &e
		if e.Set {
			out.Set, out.Value, out.Scope = true, e.Value, opvarsmodel.ScopeProject
		}
	}
	return out, nil
}

func read(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, _, ok := readScope(store, w, r)
		if !ok {
			return
		}
		key := r.PathValue("key")
		if !opvarsmodel.ValidKey(key) {
			core.WriteError(w, invalid())
			return
		}
		var out Detail
		err := store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			out, err = readDetail(ctx, tx, scope.ProjectID, key)
			return err
		})
		respond(w, out, err)
	}
}

// writeBody requires value to be present: a string sets, an explicit null
// unsets. A missing value is rejected rather than read as an unset.
type writeBody struct {
	Scope            string          `json:"scope"`
	Value            json.RawMessage `json:"value"`
	ExpectedRevision *int64          `json:"expected_revision,omitempty"`
}

type writeRequest struct {
	Key              string  `json:"key"`
	Scope            string  `json:"scope"`
	Value            *string `json:"value"`
	ExpectedRevision *int64  `json:"expected_revision,omitempty"`
}

func write(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.AdminScope(r)
		if err == nil {
			_, err = core.QueryKeys(r, "invalid_query")
		}
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var body writeBody
		if err = core.ReadJSONLimit(w, r, &body, 4<<10); err != nil {
			core.WriteError(w, err)
			return
		}
		key := r.PathValue("key")
		var value *string
		if len(body.Value) == 0 {
			core.WriteError(w, invalid())
			return
		}
		if string(body.Value) != "null" {
			var v string
			if json.Unmarshal(body.Value, &v) != nil {
				core.WriteError(w, invalid())
				return
			}
			value = &v
		}
		if !opvarsmodel.ValidKey(key) || !opvarsmodel.ValidScope(body.Scope) || value != nil && !opvarsmodel.ValidValue(*value) || body.ExpectedRevision != nil && (*body.ExpectedRevision < 0 || *body.ExpectedRevision == math.MaxInt64) {
			core.WriteError(w, invalid())
			return
		}
		idempotency, err := core.IdempotencyKey(r, "idempotency_key_required")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		req := writeRequest{key, body.Scope, value, body.ExpectedRevision}
		target := storageProject(body.Scope, scope.ProjectID)
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "operator.variable.write", Target: body.Scope + ":" + key, Key: idempotency, Request: req}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			if body.Scope == opvarsmodel.ScopeInstance {
				admin, err := core.IsInstanceAdministrator(ctx, tx, scope.PrincipalID)
				if err != nil {
					return nil, err
				}
				if !admin {
					return nil, instanceAdminRequired()
				}
			}
			var rev int64
			var value sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT revision,value FROM operator_variable_history WHERE project_id=? AND key=? ORDER BY revision DESC LIMIT 1`, target, key).Scan(&rev, &value)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if req.ExpectedRevision != nil && *req.ExpectedRevision != rev {
				return nil, conflict(rev)
			}
			if req.Value == nil && !value.Valid || req.Value != nil && value.Valid && value.String == *req.Value {
				return Receipt{key, body.Scope, rev, "unchanged"}, nil
			}
			if req.Value != nil && !value.Valid {
				var set int
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM operator_variable_history h WHERE h.project_id=? AND h.value IS NOT NULL AND h.revision=(SELECT max(revision) FROM operator_variable_history m WHERE m.project_id=h.project_id AND m.key=h.key)`, target).Scan(&set); err != nil {
					return nil, err
				}
				if set >= opvarsmodel.MaxKeys {
					return nil, tooMany()
				}
			}
			at := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
			var stored any
			if req.Value != nil {
				stored = *req.Value
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO operator_variable_history(project_id,key,revision,value,actor_id,created_at) VALUES(?,?,?,?,?,?)`, target, key, rev+1, stored, scope.PrincipalID, at); err != nil {
				return nil, err
			}
			return Receipt{key, body.Scope, rev + 1, "updated"}, nil
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

func history(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, q, ok := readScope(store, w, r, "scope", "limit", "cursor")
		if !ok {
			return
		}
		key := r.PathValue("key")
		selected, present := q.get("scope")
		if !present {
			selected = opvarsmodel.ScopeInstance
		}
		if !opvarsmodel.ValidKey(key) || !opvarsmodel.ValidScope(selected) {
			core.WriteError(w, invalid())
			return
		}
		limit := 25
		var err error
		if _, ok := q.values["limit"]; ok {
			limit, err = core.PageLimit(r)
		}
		if err != nil {
			core.WriteError(w, err)
			return
		}
		filter := "operator-variable-history:" + selected + ":" + key
		cursor, _ := q.get("cursor")
		after, err := store.ParseCursor(scope, filter, cursor)
		var before int64
		if err == nil && after != "" {
			before, err = selectedRevision(after)
		}
		if err != nil {
			core.WriteError(w, err)
			return
		}
		target := storageProject(selected, scope.ProjectID)
		out := Page{Items: make([]HistoryItem, 0, limit)}
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			query, args := `SELECT revision,value,actor_id,created_at FROM operator_variable_history WHERE project_id=? AND key=?`, []any{target, key}
			if before > 0 {
				query += ` AND revision<?`
				args = append(args, before)
			}
			rows, err := tx.QueryContext(ctx, query+` ORDER BY revision DESC LIMIT ?`, append(args, limit+1)...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var rev int64
				var value sql.NullString
				var actor, at string
				if err = rows.Scan(&rev, &value, &actor, &at); err != nil {
					return err
				}
				e, err := scanEntry(value, rev, actor, at)
				if err != nil {
					return err
				}
				if len(out.Items) == limit {
					out.HasMore = true
					break
				}
				out.Items = append(out.Items, HistoryItem{e.Revision, e.Set, e.Value, e.ActorID, e.UpdatedAt})
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
