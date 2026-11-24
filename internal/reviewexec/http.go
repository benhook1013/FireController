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
	"github.com/benhook1013/FireController/internal/apiroutes"
	"github.com/benhook1013/FireController/internal/core"
)

// Register exposes project-scoped execution controls and retained evidence.
// Provider work always runs outside these request handlers.
func (e *Engine) Register(mux apiroutes.Registrar) {
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/attempts/{attempt_id}/retained-recovery", e.recoverHostedRetainedHTTP)
	e.registerFollowingHTTP(mux)
	e.registerCIWatchHTTP(mux)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/actions-liveness/observe", e.actionsDiagnosticHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/actions-run/observe", e.actionsRunDiagnosticHTTP)
	e.registerSchedulingHTTP(mux)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/external-hosted/{pull_number}/settlement", e.settleExternalHostedHTTP)
	apiroutes.Private(mux).HandleFunc("DELETE /api/v1/operator/deployment-drain", e.deploymentDrainHTTP("cancel"))
	apiroutes.Private(mux).HandleFunc("GET /api/v1/operator/deployment-drain", e.deploymentDrainHTTP("status"))
	apiroutes.Private(mux).HandleFunc("POST /api/v1/operator/deployment-drain", e.deploymentDrainHTTP("begin"))
	adoptionholds.New(e.store).Register(mux)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/fronts", e.readFrontsHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/selections", e.enqueueRunHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/selections/{selection_id}", e.readRunHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/rounds/adoption/preview", e.previewRoundAdoptionHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/rounds/adoption/apply", e.applyRoundAdoptionHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/rounds/progress", e.readRoundProgressHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/rounds/{pull_number}/{channel}", e.readRoundControlHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/rounds/{pull_number}/{channel}", e.configureRoundControlHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/publications", e.requestCheckpointPublicationHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/publications/{publication_id}", e.readCheckpointPublicationHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/publications/{publication_id}/recovery", e.recoverCheckpointPublicationHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/readiness", e.readSourceReadinessHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/routes", e.createFindingRouteHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/routes", e.listFindingRoutesHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/routed-findings", e.readRoutedFindingsHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/routes/{route_id}", e.readFindingRouteHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/routes/{route_id}/decisions", e.decideFindingRouteHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/routes/{route_id}/fixes", e.fixFindingRouteHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/source-refreshes", e.enqueueSourceRefreshHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/source-refreshes/{refresh_id}", e.readSourceRefreshHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/policy", e.readSourcePolicyAssessmentHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/source/{pull_number}/ci", e.readSourceCIAssessmentHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/attempts/{attempt_id}/settlement", e.settleAttemptHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/policy", e.readPolicyHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/policy", e.configurePolicyHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/queue-sorting", e.queueSortingHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/queue-sorting", e.queueSortingHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/queue-priority", e.queuePriorityHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/queue-priority", e.queuePriorityHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/execution/configuration", e.readExecutionConfigurationHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/configuration", e.configureHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/add", e.changeQueueHTTP("add"))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/remove", e.changeQueueHTTP("remove"))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/move", e.changeQueueHTTP("move"))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/restore", e.changeQueueHTTP("restore"))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/execution/queue/reorder", e.changeQueueHTTP("reorder"))
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/requests", e.enqueueHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/operations", e.readOperationsHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/watch", e.reviewWatchHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/operations/{operation_id}", e.readOperationHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/operations/{operation_id}/control", e.controlOperationHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/operations/{operation_id}/status", e.readOperationStatusHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}", e.readEvidenceHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/adjudication", e.readAdjudicationHTTP)
	apiroutes.Client(mux).HandleFunc("GET /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/policy", e.readPolicyAssessmentHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/decisions", e.decideEvidenceHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/decisions/batch", e.decideEvidenceBatchHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/fixes/batch", e.fixEvidenceBatchHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/fixes", e.fixEvidenceHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/corrections", e.correctEvidenceFixHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/operational-fixes", e.operationalFixEvidenceHTTP)
	apiroutes.Client(mux).HandleFunc("POST /api/v1/projects/{project_id}/reviews/evidence/{evidence_id}/finalization", e.finalizeEvidenceHTTP)
}

func (e *Engine) configureHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "write")
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
	w.Header().Add("Vary", operationWaitHeader)
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
			operation.Wait = operationWaitForResponse(operation.Wait, operation.Channel, r.Header.Get(operationWaitHeader))
			operation.SourceRefusal = sourceRefusalForResponse(operation.SourceRefusal, r.Header.Get("X-FC-Source-Refusal"), operation.Reason)
			core.WriteJSON(w, http.StatusOK, operation)
			return
		}
	}
	var original Receipt
	if json.Unmarshal(receipt, &original) == nil {
		e.queuedResponseHeader(w, r, scope, original.ID, original.State, request.Channel, original.Revision)
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
	if r.Header.Get("X-FC-Operation-Evidence") == "true" || sourceRefusalRequested(r.Header.Get("X-FC-Source-Refusal")) || (r.Header.Get(hostedQueryDiagnosticHeader) == "1" || r.Header.Get(hostedQueryDiagnosticHeader) == "2") {
		operation, err = e.readOperationProjection(r.Context(), scope, operationID, r.Header.Get("X-FC-Operation-Evidence") == "true", sourceRefusalRequested(r.Header.Get("X-FC-Source-Refusal")), r.Header.Get("X-FC-Source-Refusal") == "v2" || r.Header.Get("X-FC-Source-Refusal") == "v3" || r.Header.Get("X-FC-Source-Refusal") == "v4", (r.Header.Get(hostedQueryDiagnosticHeader) == "1" || r.Header.Get(hostedQueryDiagnosticHeader) == "2"))
		if err != nil {
			core.WriteError(w, err)
			return
		}
	}
	// Header 1 readers know only the original allowance hold bindings. New
	// clients opt into the additional finite stage causes using header 2.
	if r.Header.Get(hostedQueryDiagnosticHeader) != "2" && operation.Reason != "hosted_allowance_unproven" && operation.Reason != "hosted_allowance_reply_pending" && operation.Reason != "hosted_allowance_cooldown" {
		operation.HostedQuery = nil
	}

	operation.PendingEvidence, err = e.pendingEvidenceForResponse(r, scope, operation.ID, operation.State, operation.Reason)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	e.queuedResponseHeader(w, r, scope, operation.ID, operation.State, operation.Channel, operation.Revision)
	e.deploymentResponseHeader(w)
	if r.Header.Get("X-FC-Hosted-Observer") == "1" {
		operation.Observer = e.observerStatus(operation)
	}
	operation.Wait = operationWaitForResponse(operation.Wait, operation.Channel, r.Header.Get(operationWaitHeader))
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
