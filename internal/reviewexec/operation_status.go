package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
)

// OperationStatus is a compact read projection, never execution or coverage authority.
type OperationStatus struct {
	PendingEvidence    *PendingEvidence `json:"pending_evidence,omitempty"`
	ID                 string           `json:"id"`
	PullNumber         int64            `json:"pull_number"`
	Channel            string           `json:"channel"`
	State              string           `json:"state"`
	Revision           int64            `json:"revision"`
	Stage              string           `json:"stage"`
	Reason             string           `json:"reason"`
	AttemptID          string           `json:"attempt_id"`
	EvidenceID         string           `json:"evidence_id,omitempty"`
	ReviewFinishedAt   string           `json:"review_finished_at,omitempty"`
	EvidenceRecordedAt string           `json:"evidence_recorded_at,omitempty"`
	RetryNotBefore     string           `json:"retry_not_before,omitempty"`
	UpdatedAt          string           `json:"updated_at"`
	NextAction         string           `json:"next_action"`
}

func operationStatusStage(o Operation, evidence string, complete bool) (string, string) {
	switch o.State {
	case "queued":
		return "queued", "wait"
	case "preparing":
		return "preparing", "wait"
	case "posting", "active":
		if evidence != "" && complete {
			return "result_captured", "wait"
		}
		return "provider_wait", "wait"
	case "completed":
		if evidence != "" {
			if complete {
				return "evidence_ready", "read_evidence"
			}
			return "capture_blocked", "contact_controller_owner"
		}
		return "unknown", "contact_controller_owner"
	case "uncertain":
		if o.Channel == "hosted" && o.Reason == "hosted_finished_inventory_unproven" {
			return "capture_blocked", "contact_controller_owner"
		}
		return "uncertain", "contact_controller_owner"
	case "held", "rate_limited":
		return "held", "read_diagnostics"
	case "failed":
		return "failed", "read_diagnostics"
	case "cancelled":
		return "cancelled", "none"
	}
	return "unknown", "contact_controller_owner"
}

// ReadOperationStatus selects one operation and at most its unique evidence row.
// Only bounded identity/anchor/completion scalars and the original validated
// Hosted cooldown deadline are projected from local journals. It never opens
// capture files, loads findings or fetches provider bodies.
func (e *Engine) ReadOperationStatus(ctx context.Context, scope core.Scope, id string) (OperationStatus, error) {
	if ctx == nil || e == nil || e.store == nil || !hostedObservationID(id, "review_operation_") {
		return OperationStatus{}, invalid()
	}
	var result OperationStatus
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		o, err := readOperationBase(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return err
		}
		if !validOperationSummary(OperationSummary{ID: o.ID, PullNumber: o.PullNumber, Channel: o.Channel, HeadSHA: o.HeadSHA, WorkerID: o.WorkerID, State: o.State, Reason: o.Reason, AttemptID: o.AttemptID, Revision: o.Revision, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}) {
			return adjudicationStorageError()
		}
		facts, err := operationStatusEvidence(ctx, tx, o)
		if err != nil {
			return err
		}
		stage, next := operationStatusStage(o, facts.ID, facts.Complete)
		result = OperationStatus{ID: o.ID, PullNumber: o.PullNumber, Channel: o.Channel, State: o.State, Revision: o.Revision, Stage: stage, Reason: o.Reason, AttemptID: o.AttemptID, EvidenceID: facts.ID, ReviewFinishedAt: facts.ReviewFinishedAt, EvidenceRecordedAt: facts.EvidenceRecordedAt, UpdatedAt: o.UpdatedAt, NextAction: next}
		if o.Channel == "hosted" && o.State == "held" && o.Reason == "hosted_allowance_cooldown" {
			allowance, err := readHeldAllowance(ctx, tx, o)
			if err != nil {
				return err
			}
			if allowance != nil {
				result.RetryNotBefore = allowance.RetryNotBefore
			}
		}
		return nil
	})
	return result, err
}

type operationEvidenceFacts struct {
	ID                 string
	Complete           bool
	ReviewFinishedAt   string
	EvidenceRecordedAt string
}

func operationStatusEvidence(ctx context.Context, tx *sql.Tx, o Operation) (operationEvidenceFacts, error) {
	if o.AttemptID == "" {
		return operationEvidenceFacts{}, nil
	}
	var attemptOperation, attemptProject, attemptAnchor string
	var id, project, operation, attempt, bodyID, bodyProject, bodyOperation, bodyAttempt, anchor, expected, fingerprint, outcome, finished, recorded sql.NullString
	var complete, hosted sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT a.operation_id,a.project_id,
 CASE WHEN length(CAST(a.anchor_json AS BLOB))<=2048 THEN a.anchor_json ELSE '' END,
 e.id,e.project_id,e.operation_id,e.attempt_id,
 json_extract(e.body_json,'$.id'),json_extract(e.body_json,'$.project_id'),json_extract(e.body_json,'$.operation_id'),json_extract(e.body_json,'$.attempt_id'),
 CASE WHEN length(CAST(json_extract(e.body_json,'$.anchor') AS BLOB))<=2048 THEN json_extract(e.body_json,'$.anchor') END,
 CASE WHEN length(CAST(json_extract(e.body_json,'$.capture.expected_anchor') AS BLOB))<=2048 THEN json_extract(e.body_json,'$.capture.expected_anchor') END,
 json_extract(e.body_json,'$.capability_fingerprint'),json_extract(e.body_json,'$.capture.review.Outcome'),CASE json_type(e.body_json,'$.capture.review.Complete') WHEN 'true' THEN 1 WHEN 'false' THEN 0 END,
 CASE WHEN json_type(e.body_json,'$.capture.finished_at')='text' AND length(CAST(json_extract(e.body_json,'$.capture.finished_at') AS BLOB))<=64 THEN json_extract(e.body_json,'$.capture.finished_at') END,
 CASE WHEN json_type(e.body_json,'$.recorded_at')='text' AND length(CAST(json_extract(e.body_json,'$.recorded_at') AS BLOB))<=64 THEN json_extract(e.body_json,'$.recorded_at') END,
 CASE WHEN e.id IS NULL THEN NULL WHEN json_type(e.body_json,'$.capture.hosted')='object' THEN 1 WHEN json_type(e.body_json,'$.capture.hosted') IS NULL OR json_type(e.body_json,'$.capture.hosted')='null' THEN 0 ELSE -1 END
 FROM review_execution_attempts a LEFT JOIN review_execution_evidence e ON e.attempt_id=a.id WHERE a.project_id=? AND a.id=?`, o.ProjectID, o.AttemptID).Scan(&attemptOperation, &attemptProject, &attemptAnchor, &id, &project, &operation, &attempt, &bodyID, &bodyProject, &bodyOperation, &bodyAttempt, &anchor, &expected, &fingerprint, &outcome, &complete, &finished, &recorded, &hosted)
	if errors.Is(err, sql.ErrNoRows) {
		return operationEvidenceFacts{}, adjudicationStorageError()
	}
	if err != nil {
		return operationEvidenceFacts{}, err
	}
	if attemptOperation != o.ID || attemptProject != o.ProjectID {
		return operationEvidenceFacts{}, adjudicationStorageError()
	}
	if !id.Valid {
		return operationEvidenceFacts{}, nil
	}
	var admitted, captured, want Anchor
	if !hostedObservationID(id.String, "review_evidence_") || project.String != o.ProjectID || operation.String != o.ID || attempt.String != o.AttemptID || bodyID.String != id.String || bodyProject.String != o.ProjectID || bodyOperation.String != o.ID || bodyAttempt.String != o.AttemptID || !anchor.Valid || !expected.Valid || !fingerprint.Valid || fingerprint.String != o.CapabilityFingerprint || !complete.Valid || complete.Int64 < 0 || complete.Int64 > 1 || !outcome.Valid || !hosted.Valid || hosted.Int64 < 0 || hosted.Int64 > 1 || (hosted.Int64 == 1) != (o.Channel == "hosted") || json.Unmarshal([]byte(attemptAnchor), &admitted) != nil || json.Unmarshal([]byte(anchor.String), &captured) != nil || json.Unmarshal([]byte(expected.String), &want) != nil || !validEvidenceAnchor(admitted) || captured != admitted || want != admitted || admitted.PullNumber != o.PullNumber || admitted.CandidateSHA != o.HeadSHA {
		return operationEvidenceFacts{}, adjudicationStorageError()
	}
	facts := operationEvidenceFacts{ID: id.String, Complete: complete.Int64 == 1 && outcome.String == "completed"}
	_, finishOK := hostedObservationTime(finished.String)
	_, recordedOK := hostedObservationTime(recorded.String)
	if finishOK && recordedOK {
		facts.ReviewFinishedAt, facts.EvidenceRecordedAt = finished.String, recorded.String
	}
	return facts, nil
}

// Waiting owns only result collection; the final compact read reauthorizes and
// rereads its unique ledger binding after the existing notification wait.
func (e *Engine) WaitOperationStatus(ctx context.Context, scope core.Scope, id string, seconds int) (OperationStatus, error) {
	if _, _, err := e.WaitOperation(ctx, scope, id, seconds); err != nil {
		return OperationStatus{}, err
	}
	return e.ReadOperationStatus(ctx, scope, id)
}
func (e *Engine) readOperationStatusHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Prefer")
	w.Header().Add("Vary", pendingEvidenceHeader)
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("operation_id")
	if !hostedObservationID(id, "review_operation_") {
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
	value, err := e.WaitOperationStatus(r.Context(), scope, id, wait)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	value.PendingEvidence, err = e.pendingEvidenceForResponse(r, scope, value.ID, value.State, value.Reason)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	e.deploymentResponseHeader(w)
	core.WriteJSON(w, http.StatusOK, value)
}
