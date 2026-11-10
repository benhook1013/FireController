package reviewexec

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"strconv"
)

func (e *Engine) registerSchedulingHTTP(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/lanes/{channel}/diagnostics", e.readLaneDiagnosticHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/scheduling", e.schedulingHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/scheduling", e.schedulingHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/pulls/{pull_number}/scheduling", e.pullSchedulingHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/pulls/{pull_number}/scheduling", e.pullSchedulingHTTP)
}
func (e *Engine) schedulingHTTP(w http.ResponseWriter, r *http.Request) {
	permission := "read"
	if r.Method == "POST" {
		permission = "write"
	}
	scope, err := e.store.Scope(r, permission)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	if r.Method == "GET" {
		out, err := e.ReadScheduling(r.Context(), scope)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, out)
		return
	}
	var input SchedulingRequest
	if err = core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := e.ConfigureScheduling(r.Context(), scope, key, input)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
func (e *Engine) pullSchedulingHTTP(w http.ResponseWriter, r *http.Request) {
	permission := "read"
	if r.Method == "POST" {
		permission = "write"
	}
	scope, err := e.store.Scope(r, permission)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	raw := r.PathValue("pull_number")
	pull, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || pull < 1 || pull > 2147483647 || strconv.FormatInt(pull, 10) != raw {
		core.WriteError(w, invalid())
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	if r.Method == "GET" {
		out, err := e.ReadPullScheduling(r.Context(), scope, pull)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, out)
		return
	}
	var wire struct {
		ExpectedRevision *int64          `json:"expected_revision"`
		Paused           *bool           `json:"paused"`
		Reason           json.RawMessage `json:"reason,omitempty"`
	}
	if err = core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	input := PullSchedulingRequest{ExpectedRevision: wire.ExpectedRevision, Paused: wire.Paused}
	if len(wire.Reason) > 0 && (string(wire.Reason) == "null" || json.Unmarshal(wire.Reason, &input.Reason) != nil) {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := e.ConfigurePullScheduling(r.Context(), scope, key, pull, input)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
