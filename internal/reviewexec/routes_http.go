package reviewexec

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

var routeHTTPID = regexp.MustCompile(`^review_route_[0-9a-f]{32}$`)

func (e *Engine) createFindingRouteHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var wire struct {
		ExpectedRevision      *int64  `json:"expected_revision"`
		Finding               *int64  `json:"finding"`
		TargetPullNumber      *int64  `json:"target_pull_number"`
		Reason                *string `json:"reason"`
		ExpectedRouteRevision *int64  `json:"expected_route_revision,omitempty"`
	}
	wire.ExpectedRouteRevision = new(int64)
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedRevision == nil || wire.Finding == nil || wire.TargetPullNumber == nil || wire.Reason == nil || wire.ExpectedRouteRevision == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.CreateFindingRoute(r.Context(), scope, key, id, FindingRouteRequest{ExpectedRevision: wire.ExpectedRevision, Finding: *wire.Finding, TargetPullNumber: *wire.TargetPullNumber, Reason: *wire.Reason, ExpectedRouteRevision: *wire.ExpectedRouteRevision})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func (e *Engine) routeHTTPScope(w http.ResponseWriter, r *http.Request, permission string) (core.Scope, string, bool) {
	scope, err := e.store.Scope(r, permission)
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, "", false
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return core.Scope{}, "", false
	}
	id := r.PathValue("route_id")
	if !routeHTTPID.MatchString(id) {
		core.WriteError(w, invalid())
		return core.Scope{}, "", false
	}
	return scope, id, true
}

func (e *Engine) readFindingRouteHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.routeHTTPScope(w, r, "read")
	if !ok {
		return
	}
	value, err := e.ReadFindingRoute(r.Context(), scope, id)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	f := value.SourceFinding
	core.WriteJSON(w, http.StatusOK, struct {
		Route         ReviewRoute           `json:"route"`
		SourceAnchor  Anchor                `json:"source_anchor"`
		SourceFinding sourceFindingResponse `json:"source_finding"`
	}{value.Route, value.SourceAnchor, sourceFindingResponse{Finding: value.Route.Finding, Title: f.Title, Detail: f.Detail, Severity: f.Severity, Path: f.Path, StartLine: f.StartLine, EndLine: f.EndLine}})
}

func (e *Engine) listFindingRoutesHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	query, err := core.QueryKeys(r, "validation_failed", "pull_number", "limit", "cursor")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	raw := query.Get("pull_number")
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != raw {
		core.WriteError(w, invalid())
		return
	}
	limit := 5
	if _, present := query["limit"]; present {
		limit, err = core.PageLimit(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
	}
	value, err := e.ListFindingRoutes(r.Context(), scope, number, limit, query.Get("cursor"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func (e *Engine) decideFindingRouteHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.routeHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var wire struct {
		ExpectedRevision *int64  `json:"expected_revision"`
		Disposition      *string `json:"disposition"`
		Reason           *string `json:"reason"`
	}
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedRevision == nil || wire.Disposition == nil || wire.Reason == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.DecideFindingRoute(r.Context(), scope, key, id, FindingRouteDecisionRequest{ExpectedRevision: wire.ExpectedRevision, Disposition: *wire.Disposition, Reason: *wire.Reason})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}

func (e *Engine) fixFindingRouteHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.routeHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var wire struct {
		ExpectedRevision *int64  `json:"expected_revision"`
		ExpectedSHA      *string `json:"expected_sha"`
		FixSHA           *string `json:"fix_sha"`
		ProofNote        *string `json:"proof_note"`
	}
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedRevision == nil || wire.ExpectedSHA == nil || wire.FixSHA == nil || wire.ProofNote == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value, err := e.RecordFindingRouteFix(r.Context(), scope, key, id, FindingRouteFixRequest{ExpectedRevision: wire.ExpectedRevision, ExpectedSHA: wire.ExpectedSHA, FixSHA: *wire.FixSHA, ProofNote: *wire.ProofNote})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, value)
}
