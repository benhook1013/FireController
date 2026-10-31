package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/core"
)

// Register exposes project-scoped execution controls and retained evidence.
// Provider work always runs outside these request handlers.
func (e *Engine) Register(mux *http.ServeMux) {
	e.registerCIWatchHTTP(mux)
	mux.HandleFunc("DELETE /api/v1/operator/deployment-drain", e.deploymentDrainHTTP("cancel"))
	mux.HandleFunc("GET /api/v1/operator/deployment-drain", e.deploymentDrainHTTP("status"))
	mux.HandleFunc("POST /api/v1/operator/deployment-drain", e.deploymentDrainHTTP("begin"))
	adoptionholds.New(e.store).Register(mux)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/fronts", e.readFrontsHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/selections", e.enqueueRunHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/selections/{selection_id}", e.readRunHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/rounds/adoption/preview", e.previewRoundAdoptionHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/rounds/adoption/apply", e.applyRoundAdoptionHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/rounds/progress", e.readRoundProgressHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/rounds/{pull_number}/{channel}", e.readRoundControlHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/rounds/{pull_number}/{channel}", e.configureRoundControlHTTP)
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
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/attempts/{attempt_id}/settlement", e.settleAttemptHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/policy", e.readPolicyHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/policy", e.configurePolicyHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/execution/configuration", e.readExecutionConfigurationHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/configuration", e.configureHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/add", e.changeQueueHTTP("add"))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/remove", e.changeQueueHTTP("remove"))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/move", e.changeQueueHTTP("move"))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/requests", e.enqueueHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/operations", e.readOperationsHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/operations/{operation_id}", e.readOperationHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/operations/{operation_id}/control", e.controlOperationHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/operations/{operation_id}/status", e.readOperationStatusHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}", e.readEvidenceHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/adjudication", e.readAdjudicationHTTP)
	mux.HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/policy", e.readPolicyAssessmentHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/decisions", e.decideEvidenceHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/decisions/batch", e.decideEvidenceBatchHTTP)
	mux.HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/fixes/batch", e.fixEvidenceBatchHTTP)
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
	w.Header().Add("Vary", "Prefer")
	w.Header().Add("Vary", pendingEvidenceHeader)
	scope, err := e.store.Scope(r, "write")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	wait, err := operationPreference(r, 2)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if wait > 0 {
		if err = e.store.Read(r.Context(), scope, func(context.Context, *sql.Tx) error { return nil }); err != nil {
			core.WriteError(w, err)
			return
		}
	}
	var wire struct {
		ExpectedConfigurationRevision int64           `json:"expected_configuration_revision"`
		PullNumber                    int64           `json:"pull_number"`
		Channel                       string          `json:"channel"`
		HeadSHA                       string          `json:"head_sha"`
		AllowOutOfOrder               json.RawMessage `json:"allow_out_of_order,omitempty"`
		OverrideFileLimit             json.RawMessage `json:"override_file_limit,omitempty"`
	}
	if err := core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	flag := bytes.TrimSpace(wire.AllowOutOfOrder)
	if len(flag) != 0 && !bytes.Equal(flag, []byte("true")) && !bytes.Equal(flag, []byte("false")) {
		core.WriteError(w, invalid())
		return
	}
	limitFlag := bytes.TrimSpace(wire.OverrideFileLimit)
	if len(limitFlag) != 0 && !bytes.Equal(limitFlag, []byte("true")) && !bytes.Equal(limitFlag, []byte("false")) {
		core.WriteError(w, invalid())
		return
	}
	request := Request{ExpectedConfigurationRevision: wire.ExpectedConfigurationRevision, PullNumber: wire.PullNumber, Channel: wire.Channel, HeadSHA: wire.HeadSHA, AllowOutOfOrder: bytes.Equal(flag, []byte("true")), OverrideFileLimit: bytes.Equal(limitFlag, []byte("true"))}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err = operationResponseDeadline(w, wait); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(wait))
	receipt, err := e.Enqueue(r.Context(), scope, key, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if wait > 0 {
		var queued Receipt
		if json.Unmarshal(receipt, &queued) != nil {
			writeOperationWaitError(w, core.Fail("storage_unavailable", "The retained review receipt is unavailable.", 500))
			return
		}
		operation, terminal, waitErr := e.WaitOperation(r.Context(), scope, queued.ID, wait)
		if waitErr != nil {
			if detached, detachErr := e.acceptedWaitDetached(r.Context(), scope, waitErr); detached {
				e.deploymentResponseHeader(w)
				core.WriteJSON(w, http.StatusAccepted, receipt)
				return
			} else {
				waitErr = detachErr
			}
			var failure *core.Error
			if errors.As(waitErr, &failure) && failure.Code == "overloaded" {
				if details, ok := failure.Details.(map[string]string); ok && details["reason"] == "operation_wait_capacity" {
					operation, waitErr = e.ReadOperation(r.Context(), scope, queued.ID)
					terminal = waitErr == nil && !operationPending(operation.State)
				}
			}
			if waitErr != nil {
				writeOperationWaitError(w, waitErr)
				return
			}
		}
		if terminal {
			if r.Header.Get("X-FC-Operation-Evidence") == "true" || sourceRefusalRequested(r.Header.Get("X-FC-Source-Refusal")) {
				operation, err = e.readOperationProjection(r.Context(), scope, operation.ID, r.Header.Get("X-FC-Operation-Evidence") == "true", sourceRefusalRequested(r.Header.Get("X-FC-Source-Refusal")), r.Header.Get("X-FC-Source-Refusal") == "v2" || r.Header.Get("X-FC-Source-Refusal") == "v3" || r.Header.Get("X-FC-Source-Refusal") == "v4")
				if err != nil {
					writeOperationWaitError(w, err)
					return
				}
			}
			operation.PendingEvidence, err = e.pendingEvidenceForResponse(r, scope, operation.ID, operation.State, operation.Reason)
			if err != nil {
				writeOperationWaitError(w, err)
				return
			}
			e.deploymentResponseHeader(w)
			if r.Header.Get(operationWaitHeader) != "1" {
				operation.Wait = nil
			}
			operation.SourceRefusal = sourceRefusalForResponse(operation.SourceRefusal, r.Header.Get("X-FC-Source-Refusal"), operation.Reason)
			core.WriteJSON(w, http.StatusOK, operation)
			return
		}
	}
	var original Receipt
	if json.Unmarshal(receipt, &original) == nil {
		e.queuedResponseHeader(w, r, scope, original.ID, original.State, original.Revision)
	}
	e.deploymentResponseHeader(w)
	core.WriteJSON(w, http.StatusAccepted, receipt)
}

func (e *Engine) readOperationHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "X-FC-Hosted-Observer")
	w.Header().Add("Vary", hostedQueryDiagnosticHeader)
	w.Header().Add("Vary", operationWaitHeader)
	w.Header().Add("Vary", "Prefer")
	w.Header().Add("Vary", pendingEvidenceHeader)
	w.Header().Add("Vary", "X-FC-Operation-Evidence")
	w.Header().Add("Vary", "X-FC-Source-Refusal")
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
	wait, err := operationPreference(r, 0)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if err = operationResponseDeadline(w, wait); err != nil {
		core.WriteError(w, err)
		return
	}
	w.Header().Set("Preference-Applied", "wait="+strconv.Itoa(wait))
	operation, _, err := e.WaitOperation(r.Context(), scope, operationID, wait)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if r.Header.Get("X-FC-Operation-Evidence") == "true" || sourceRefusalRequested(r.Header.Get("X-FC-Source-Refusal")) || r.Header.Get(hostedQueryDiagnosticHeader) == "1" {
		operation, err = e.readOperationProjection(r.Context(), scope, operationID, r.Header.Get("X-FC-Operation-Evidence") == "true", sourceRefusalRequested(r.Header.Get("X-FC-Source-Refusal")), r.Header.Get("X-FC-Source-Refusal") == "v2" || r.Header.Get("X-FC-Source-Refusal") == "v3" || r.Header.Get("X-FC-Source-Refusal") == "v4", r.Header.Get(hostedQueryDiagnosticHeader) == "1")
		if err != nil {
			core.WriteError(w, err)
			return
		}
	}
	operation.PendingEvidence, err = e.pendingEvidenceForResponse(r, scope, operation.ID, operation.State, operation.Reason)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	e.queuedResponseHeader(w, r, scope, operation.ID, operation.State, operation.Revision)
	e.deploymentResponseHeader(w)
	if r.Header.Get("X-FC-Hosted-Observer") == "1" {
		operation.Observer = e.observerStatus(operation)
	}
	if r.Header.Get(operationWaitHeader) != "1" {
		operation.Wait = nil
	}
	operation.SourceRefusal = sourceRefusalForResponse(operation.SourceRefusal, r.Header.Get("X-FC-Source-Refusal"), operation.Reason)
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
