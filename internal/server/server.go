// Package server exposes the bounded private coordination API.
package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

type RepositoryIdentity struct {
	ID       int64
	FullName string
}
type RepositoryResolver func(context.Context, string) (RepositoryIdentity, error)
type Option func(*api)
type api struct {
	store              *core.Store
	resolver           RepositoryResolver
	browser            http.Handler
	backupHealthFiles  []string
	backupHealthReader BackupHealthReader
}

func WithRepositoryResolver(resolver RepositoryResolver) Option {
	return func(a *api) { a.resolver = resolver }
}

// WithBrowser installs the embedded browser without changing scoped API routes.
func WithBrowser(handler http.Handler) Option {
	return func(a *api) { a.browser = handler }
}

func New(store *core.Store, options ...Option) *http.ServeMux {
	a := &api{store: store}
	for _, option := range options {
		option(a)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		core.WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.DB().PingContext(r.Context()); err != nil {
			core.WriteError(w, core.Fail("storage_unavailable", "Local storage is unavailable.", 503))
			return
		}
		core.WriteJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /api/v1/instance", func(w http.ResponseWriter, r *http.Request) {
		if _, err := store.Authenticate(r); err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, map[string]string{"instance_id": store.InstanceID()})
	})
	mux.HandleFunc("POST /api/v1/projects", a.createProject)
	mux.HandleFunc("GET /api/v1/projects", a.listProjects)
	mux.HandleFunc("GET /api/v1/projects/{project_id}", a.project)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/backup-health", a.backupHealth)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/browser-session", a.createBrowserSession)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/browser-session", a.currentBrowserSession)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/browser-session/renew", a.renewBrowserSession)
	mux.HandleFunc("DELETE /api/v1/projects/{project_id}/browser-session", a.deleteBrowserSession)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/workers", a.createWorker)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/workers", a.listWorkers)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/workers/{worker_id}", a.worker)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/workers/presence", a.workerPresence)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/message-events", a.messageEvents)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/credentials", a.grant)
	mux.HandleFunc("DELETE /api/v1/projects/{project_id}/credentials/{credential_id}", a.revoke)
	if a.browser != nil {
		mux.Handle("/", a.browser)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			core.WriteError(w, core.Fail("not_found", "This API capability is unavailable.", 404))
		})
	}
	return mux
}

func boundedText(value string, max int) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= max && utf8.ValidString(value)
}

func key(r *http.Request) (string, error) {
	return core.IdempotencyKey(r, "validation_failed")
}

func complete(w http.ResponseWriter, result any, err error) {
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, result)
}

func listQuery(r *http.Request) (url.Values, error) {
	invalid := core.Fail("validation_failed", "Use only one limit and one cursor query parameter.", 400)
	if len(r.URL.RawQuery) > 16<<10 {
		return nil, invalid
	}
	return core.QueryKeys(r, "validation_failed", "limit", "cursor")
}

type projectValue struct {
	ID           string `json:"id"`
	Handle       string `json:"handle"`
	Name         string `json:"name"`
	RepositoryID int64  `json:"github_repository_id"`
	Repository   string `json:"repository"`
}
type workerValue struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
}

func (a *api) createProject(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var request struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
		Handle     string `json:"handle,omitempty"`
	}
	if err = core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	if !boundedText(request.Name, 200) || strings.IndexFunc(request.Name, unicode.IsControl) >= 0 || !validRepository(request.Repository) || request.Handle != "" && !core.ValidProjectHandle(request.Handle) {
		core.WriteError(w, core.Fail("validation_failed", "Provide a bounded project name, an owner/repository GitHub reference, and an optional lowercase handle.", 400))
		return
	}
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	mutation := core.Mutation{Operation: "projects.create", Target: "projects", Key: k, Request: request}
	if receipt, found, err := a.store.Replay(r.Context(), scope, mutation); err != nil || found {
		complete(w, receipt, err)
		return
	}
	if request.Handle != "" {
		var exists int
		err = a.store.DB().QueryRowContext(r.Context(), `SELECT 1 FROM project_handles WHERE handle=? UNION ALL SELECT 1 FROM projects WHERE id=? LIMIT 1`, request.Handle, request.Handle).Scan(&exists)
		if err == nil {
			core.WriteError(w, core.Fail("project_handle_conflict", "This project handle is already in use.", 409))
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			core.WriteError(w, err)
			return
		}
	}
	if a.resolver == nil {
		core.WriteError(w, core.Fail("provider_unavailable", "Repository verification is not configured.", 503))
		return
	}
	// Network identity verification is explicit and never occupies a transaction.
	identity, err := a.resolver(r.Context(), request.Repository)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "repository_not_found" && failure.Status == http.StatusNotFound {
			core.WriteError(w, core.Fail("repository_not_found", "The requested GitHub repository was not found.", http.StatusNotFound))
			return
		}
		core.WriteError(w, core.Fail("provider_unavailable", "GitHub repository identity could not be verified.", 503))
		return
	}
	if identity.ID <= 0 || !validRepository(identity.FullName) {
		core.WriteError(w, core.Fail("provider_unavailable", "GitHub repository identity could not be verified.", 503))
		return
	}
	result, err := a.store.Mutate(r.Context(), scope, mutation, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var existing string
		err := tx.QueryRowContext(ctx, "SELECT id FROM projects WHERE github_repository_id=?", identity.ID).Scan(&existing)
		if err == nil {
			return nil, &core.Error{Code: "repository_already_configured", Message: "This repository already belongs to a project.", Status: 409, Details: map[string]string{"project_id": existing}}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		id := core.NewID("project")
		if _, err = tx.ExecContext(ctx, "INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)", id, request.Name, identity.ID, identity.FullName); err != nil {
			return nil, err
		}
		handle, err := core.AssignProjectHandle(ctx, tx, id, request.Name, request.Handle)
		if err != nil {
			return nil, err
		}
		eventScope := scope
		eventScope.ProjectID = id
		if err = core.AppendEvent(ctx, tx, eventScope, "projects.created", id, 1, map[string]string{"id": id}); err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "handle": handle, "revision": 1, "outcome": "created"}, nil
	})
	complete(w, result, err)
}

func (a *api) project(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var value projectValue
	err = a.store.DB().QueryRowContext(r.Context(), "SELECT p.id,h.handle,p.name,p.github_repository_id,p.repository FROM projects p JOIN project_handles h ON h.project_id=p.id WHERE p.id=?", scope.ProjectID).Scan(&value.ID, &value.Handle, &value.Name, &value.RepositoryID, &value.Repository)
	complete(w, value, err)
}

func (a *api) listProjects(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	query, err := listQuery(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	limit, err := core.PageLimit(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	after, err := a.store.ParseCursor(scope, "projects", query.Get("cursor"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	rows, err := a.store.DB().QueryContext(r.Context(), "SELECT p.id,h.handle,p.name,p.github_repository_id,p.repository FROM projects p JOIN project_handles h ON h.project_id=p.id WHERE p.id>? ORDER BY p.id LIMIT ?", after, limit+1)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	defer rows.Close()
	values := make([]projectValue, 0, limit+1)
	for rows.Next() {
		var value projectValue
		if err = rows.Scan(&value.ID, &value.Handle, &value.Name, &value.RepositoryID, &value.Repository); err != nil {
			core.WriteError(w, err)
			return
		}
		values = append(values, value)
	}
	if err = rows.Err(); err != nil {
		core.WriteError(w, err)
		return
	}
	more := len(values) > limit
	cursor := ""
	if more {
		values = values[:limit]
		cursor = a.store.Cursor(scope, "projects", values[len(values)-1].ID)
	}
	core.WriteJSON(w, 200, struct {
		Items   []projectValue `json:"items"`
		Cursor  string         `json:"cursor,omitempty"`
		HasMore bool           `json:"has_more"`
	}{values, cursor, more})
}

func (a *api) createWorker(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if err = core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	if !boundedText(request.Name, 200) {
		core.WriteError(w, core.Fail("validation_failed", "Provide a bounded worker name without surrounding whitespace.", 400))
		return
	}
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := a.store.Mutate(r.Context(), scope, core.Mutation{Operation: "workers.create", Target: "workers", Key: k, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var exists int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM workers WHERE project_id=? AND name=?", scope.ProjectID, request.Name).Scan(&exists)
		if err == nil {
			return nil, core.Fail("worker_already_configured", "A worker with this name already exists in the selected project.", 409)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		id := core.NewID("worker")
		if _, err = tx.ExecContext(ctx, "INSERT INTO workers(project_id,id,name) VALUES(?,?,?)", scope.ProjectID, id, request.Name); err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "workers.created", id, 1, map[string]string{"id": id}); err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "revision": 1, "outcome": "created"}, nil
	})
	complete(w, result, err)
}

func (a *api) listWorkers(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	query, err := listQuery(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	limit, err := core.PageLimit(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	filter := "workers"
	after, err := a.store.ParseCursor(scope, filter, query.Get("cursor"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	rows, err := a.store.DB().QueryContext(r.Context(), "SELECT id,project_id,name FROM workers WHERE project_id=? AND id>? ORDER BY id LIMIT ?", scope.ProjectID, after, limit+1)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	defer rows.Close()
	values := make([]workerValue, 0, limit+1)
	for rows.Next() {
		var value workerValue
		if err = rows.Scan(&value.ID, &value.ProjectID, &value.Name); err != nil {
			core.WriteError(w, err)
			return
		}
		values = append(values, value)
	}
	if err = rows.Err(); err != nil {
		core.WriteError(w, err)
		return
	}
	more := len(values) > limit
	cursor := ""
	if more {
		values = values[:limit]
		cursor = a.store.Cursor(scope, filter, values[len(values)-1].ID)
	}
	core.WriteJSON(w, 200, struct {
		Items   []workerValue `json:"items"`
		Cursor  string        `json:"cursor,omitempty"`
		HasMore bool          `json:"has_more"`
	}{values, cursor, more})
}

func (a *api) worker(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var value workerValue
	err = a.store.DB().QueryRowContext(r.Context(), "SELECT id,project_id,name FROM workers WHERE project_id=? AND id=?", scope.ProjectID, r.PathValue("worker_id")).Scan(&value.ID, &value.ProjectID, &value.Name)
	if errors.Is(err, sql.ErrNoRows) {
		err = core.Fail("not_found", "The worker is unavailable in this project.", 404)
	}
	complete(w, value, err)
}
