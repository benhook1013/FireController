// Package reviews exposes the administrator import and scoped read API for the
// immutable native review observation workspace.
package reviews

import (
	"net/http"

	"github.com/benhook1013/FireController/internal/apiroutes"
	"github.com/benhook1013/FireController/internal/core"
)

// Register mounts the review observation write boundary and its provider-free
// project-scoped read routes.
func Register(mux apiroutes.Registrar, store *core.Store, health ...ObservationHealthReader) {
	registerSubagentReports(mux, store)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/activity", activity(store))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/transfer/preview", previewTransfer(store))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/transfer/apply", applyTransfer(store))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/import/preview", previewImport(store))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/import/apply", applyImport(store))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/observations", func(w http.ResponseWriter, r *http.Request) {
		appendObservation(store, w, r)
	})
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/queue/edits", editQueue(store))
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/queue", queue(store, health...))
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/queue/active", activeQueue(store))
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{number}", pull(store))
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{number}/runs", runs(store))
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{number}/attempts", attempts(store))
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/runs/{run_id}", runDetail(store))
}
