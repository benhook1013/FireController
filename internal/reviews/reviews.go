// Package reviews exposes the administrator import and scoped read API for the
// immutable native review observation workspace.
package reviews

import (
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

// Register mounts the review observation write boundary and its provider-free
// project-scoped read routes.
func Register(mux *http.ServeMux, store *core.Store) {
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/import/preview", previewImport(store))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/import/apply", applyImport(store))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/observations", func(w http.ResponseWriter, r *http.Request) {
		appendObservation(store, w, r)
	})
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/queue", queue(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{number}", pull(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{number}/runs", runs(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{number}/attempts", attempts(store))
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/runs/{run_id}", runDetail(store))
}
