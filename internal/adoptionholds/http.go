package adoptionholds

import (
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"strconv"
)

func (s *Service) Register(mux *http.ServeMux) {
	const base = "/api/v1/projects/{project_id}/reviews/adoption-obligations"
	mux.HandleFunc("GET "+base, s.listHTTP)
	mux.HandleFunc("POST "+base+"/preview", s.previewHTTP)
	mux.HandleFunc("POST "+base+"/apply", s.applyHTTP)
	mux.HandleFunc("GET "+base+"/{id}", s.detailHTTP)
	mux.HandleFunc("POST "+base+"/{id}/resolve", s.resolveHTTP)
}
func (s *Service) scope(w http.ResponseWriter, r *http.Request, admin bool) (core.Scope, bool) {
	var scope core.Scope
	var err error
	if admin {
		scope, err = s.store.AdminScope(r)
		if err == nil {
			err = administrative(scope)
		}
	} else {
		scope, err = s.store.Scope(r, "read")
	}
	if err == nil {
		_, err = core.QueryKeys(r, "validation_failed")
	}
	if err != nil {
		core.WriteError(w, err)
		return scope, false
	}
	return scope, true
}
func (s *Service) previewHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.scope(w, r, true)
	if !ok {
		return
	}
	var batch m.Batch
	if err := core.ReadJSON(w, r, &batch); err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := s.Preview(r.Context(), scope, batch)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
func (s *Service) applyHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.scope(w, r, true)
	if !ok {
		return
	}
	var input m.ApplyRequest
	if err := core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := s.Apply(r.Context(), scope, key, input)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
func (s *Service) resolveHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := s.store.Scope(r, "write")
	if err == nil {
		_, err = core.QueryKeys(r, "validation_failed")
	}
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var input m.ResolveRequest
	if err := core.ReadJSON(w, r, &input); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := s.Resolve(r.Context(), scope, r.PathValue("id"), key, input)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
func (s *Service) detailHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.scope(w, r, false)
	if !ok {
		return
	}
	out, err := s.Detail(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
func (s *Service) listHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := s.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	values, err := core.QueryKeys(r, "validation_failed", "pull_number", "status", "limit", "cursor")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	limit, err := core.PageLimit(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	number := int64(0)
	if raw, exists := values["pull_number"]; exists {
		number, err = strconv.ParseInt(raw[0], 10, 32)
		if err != nil || !pull(number) || strconv.FormatInt(number, 10) != raw[0] {
			core.WriteError(w, invalid())
			return
		}
	}
	status := values.Get("status")
	if status == "" {
		status = "open"
	}
	out, err := s.List(r.Context(), scope, number, status, limit, values.Get("cursor"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, out)
}
