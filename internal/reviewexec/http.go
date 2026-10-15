package reviewexec

import (
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
)

// Register exposes project-scoped execution controls and retained evidence.
// Provider work always runs outside these request handlers.
func (e *Engine) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/publications", e.requestCheckpointPublicationHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/publications/{publication_id}", e.readCheckpointPublicationHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/publications/{publication_id}/recovery", e.recoverCheckpointPublicationHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/readiness", e.readSourceReadinessHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/routes", e.createFindingRouteHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/routes", e.listFindingRoutesHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/routes/{route_id}", e.readFindingRouteHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/routes/{route_id}/decisions", e.decideFindingRouteHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/routes/{route_id}/fixes", e.fixFindingRouteHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/source-refreshes", e.enqueueSourceRefreshHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/source-refreshes/{refresh_id}", e.readSourceRefreshHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/policy", e.readSourcePolicyAssessmentHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/ci", e.readSourceCIAssessmentHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/policy", e.readPolicyHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/policy", e.configurePolicyHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/execution/configuration", e.readExecutionConfigurationHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/configuration", e.configureHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/requests", e.enqueueHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/operations/{operation_id}", e.readOperationHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}", e.readEvidenceHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/adjudication", e.readAdjudicationHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/policy", e.readPolicyAssessmentHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/decisions", e.decideEvidenceHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/fixes", e.fixEvidenceHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/finalization", e.finalizeEvidenceHTTP)
}

func (e *Engine) configureHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var request Configuration
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	receipt, err := e.Configure(r.Context(), scope, key, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusCreated, receipt)
}

func (e *Engine) enqueueHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var request Request
	if err := core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	receipt, err := e.Enqueue(r.Context(), scope, key, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusAccepted, receipt)
}

func (e *Engine) readOperationHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	operationID := r.PathValue("operation_id")
	if !identifier.MatchString(operationID) {
		core.WriteError(w, invalid())
		return
	}
	operation, err := e.ReadOperation(r.Context(), scope, operationID)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, operation)
}

func (e *Engine) readExecutionConfigurationHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	configuration, err := e.ReadExecutionConfiguration(r.Context(), scope)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, configuration)
}
