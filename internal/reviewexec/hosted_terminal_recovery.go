package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// HostedRetainedDecoder is a trusted, provider-free application integration.
// Requests cannot supply findings, capture bytes, paths or decoder output.
type HostedRetainedDecoder func(context.Context, *Engine, string, string, string, string) (ReviewEvidenceInput, error)

func WithHostedRetainedDecoder(decoder HostedRetainedDecoder) EngineOption {
	return func(e *Engine) { e.hostedRetainedDecoder = decoder }
}

type hostedRetainedRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	OperationID      string `json:"operation_id"`
	CapabilityID     string `json:"capability_id"`
	RepositoryID     int64  `json:"repository_id"`
	HeadSHA          string `json:"head_sha"`
	ExternalID       string `json:"external_id"`
	CaptureSHA256    string `json:"capture_sha256"`
}
type hostedRetainedReceipt struct {
	AttemptID   string `json:"attempt_id"`
	OperationID string `json:"operation_id"`
	EvidenceID  string `json:"evidence_id"`
	Revision    int64  `json:"revision"`
	State       string `json:"state"`
}

func retainedRecoveryUnavailable() error {
	return core.Fail("hosted_retained_recovery_unavailable", "Only an audited incomplete release with unchanged original source and no pending replacement can be recovered.", 409)
}

func (e *Engine) recoverHostedRetainedHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var request hostedRetainedRequest
	if err = core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("attempt_id")
	if !hostedObservationID(id, "review_attempt_") || request.ExpectedRevision < 3 || request.ExpectedRevision == math.MaxInt64 || !hostedObservationID(request.OperationID, "review_operation_") || !identifier.MatchString(request.CapabilityID) || request.RepositoryID <= 0 || !sha.MatchString(request.HeadSHA) || !identifier.MatchString(request.ExternalID) || !hash.MatchString(request.CaptureSHA256) {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	mutation := core.Mutation{Operation: "reviews.hosted.retained-recovery", Target: id, Key: key, Request: request}
	receipt, found, err := e.store.Replay(r.Context(), scope, mutation)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if !found {
		if e.hostedRetainedDecoder == nil {
			core.WriteError(w, retainedRecoveryUnavailable())
			return
		}
		input, decodeErr := e.hostedRetainedDecoder(r.Context(), e, scope.ProjectID, id, request.CapabilityID, request.CaptureSHA256)
		if decodeErr != nil {
			// Another exact-key caller may have completed while this decoder
			// read its original state. Reauthorize and recover its receipt.
			receipt, found, err = e.store.Replay(r.Context(), scope, mutation)
			if err != nil {
				core.WriteError(w, err)
				return
			}
			if !found {
				core.WriteError(w, decodeErr)
				return
			}
		} else {
			receipt, err = e.store.Mutate(r.Context(), scope, mutation, func(ctx context.Context, tx *sql.Tx) (any, error) {
				return e.recoverHostedRetainedTx(ctx, tx, scope, id, request, input)
			})
			if err != nil {
				core.WriteError(w, err)
				return
			}
		}
	}
	var recovered hostedRetainedReceipt
	if json.Unmarshal(receipt, &recovered) != nil {
		core.WriteError(w, hostedRecoveryUnavailable())
		return
	}
	e.notifyOperation(recovered.OperationID)
	// Publication is replayable from the same immutable receipt. A missing current
	// PR projection does not invalidate historical execution; finalization remains
	// a separate ordinary action and no review-round grant is changed here.
	if _, err = e.PublishEvidence(r.Context(), scope.ProjectID, recovered.EvidenceID, nil); err != nil {
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "review_projection_missing" {
			core.WriteError(w, err)
			return
		}
	}
	core.WriteJSON(w, http.StatusOK, json.RawMessage(receipt))
}

func (e *Engine) recoverHostedRetainedTx(ctx context.Context, tx *sql.Tx, scope core.Scope, id string, request hostedRetainedRequest, input ReviewEvidenceInput) (hostedRetainedReceipt, error) {
	empty := hostedRetainedReceipt{}
	a, o, err := readHostedPostingAttempt(ctx, tx, scope.ProjectID, id)
	if err != nil {
		return empty, err
	}
	if a.State != "failed" || o.State != a.State || o.Reason != HostedFinishedIncompleteReason || a.Revision != request.ExpectedRevision || o.Revision == math.MaxInt64 || o.ID != request.OperationID || a.Anchor == nil || a.Anchor.RepositoryID != request.RepositoryID || a.Anchor.CandidateSHA != request.HeadSHA || a.ExternalID != request.ExternalID {
		return empty, retainedRecoveryUnavailable()
	}
	cap, ok := e.capabilities[request.CapabilityID]
	var configBody string
	if err = tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=?`, scope.ProjectID, o.ConfigurationRevision).Scan(&configBody); err != nil {
		return empty, err
	}
	var config Configuration
	if !ok || !slices.Contains(cap.Channels, "hosted") || cap.Fingerprint != o.CapabilityFingerprint || cap.Provider != a.Provider || cap.Account != a.Account || json.Unmarshal([]byte(configBody), &config) != nil || validateConfigurationShape(config) != nil || encoded(config) != configBody || config.CapabilityID != cap.ID {
		return empty, retainedRecoveryUnavailable()
	}
	if err = validateConfirmedHostedSettlement(ctx, tx, a, o); err != nil {
		return empty, err
	}
	if err = validateSettlementAdmission(ctx, tx, a, o, o.UpdatedAt); err != nil {
		return empty, err
	}
	if err = validateSettlementObservations(ctx, tx, a, o, false); err != nil {
		return empty, err
	}
	var transitionState, transitionReason, transitionExternal, transitionAt string
	if err = tx.QueryRowContext(ctx, `SELECT state,reason,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&transitionState, &transitionReason, &transitionExternal, &transitionAt); err != nil {
		return empty, err
	}
	if transitionState != a.State || transitionReason != o.Reason || transitionExternal != a.ExternalID || transitionAt != o.UpdatedAt {
		return empty, retainedRecoveryUnavailable()
	}
	var release struct {
		OperationID     string `json:"operation_id"`
		ExternalID      string `json:"external_id"`
		CaptureID       string `json:"capture_id"`
		CaptureSHA256   string `json:"capture_sha256"`
		FirstObservedAt string `json:"first_observed_at"`
		ReleasedAt      string `json:"released_at"`
	}
	var auditBody string
	var auditCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(max(payload_json),'') FROM events WHERE project_id=? AND event_type='review.hosted_incomplete_released' AND resource_id=? AND revision=?`, scope.ProjectID, a.ID, a.Revision).Scan(&auditCount, &auditBody); err != nil {
		return empty, err
	}
	if auditCount != 1 || len(auditBody) > 4096 || json.Unmarshal([]byte(auditBody), &release) != nil || release.OperationID != o.ID || release.ExternalID != a.ExternalID || release.CaptureSHA256 != request.CaptureSHA256 || release.CaptureID != input.CaptureID {
		return empty, retainedRecoveryUnavailable()
	}
	first, firstOK := attemptRecordedTime(release.FirstObservedAt)
	released, releasedOK := attemptRecordedTime(release.ReleasedAt)
	terminal, terminalOK := attemptRecordedTime(transitionAt)
	if !firstOK || !releasedOK || !terminalOK || released.Before(first.Add(HostedIncompleteGrace)) || terminal.Before(released) {
		return empty, retainedRecoveryUnavailable()
	}
	var priorState, priorReason, priorAt string
	if err = tx.QueryRowContext(ctx, `SELECT state,reason,created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision-1).Scan(&priorState, &priorReason, &priorAt); err != nil {
		return empty, err
	}
	if priorState != "uncertain" || priorReason != "hosted_finished_inventory_unproven" || priorAt != release.FirstObservedAt {
		return empty, retainedRecoveryUnavailable()
	}
	var observationBody, checksum string
	var captureBytes int64
	if err = tx.QueryRowContext(ctx, `SELECT body_json,capture_sha256,capture_bytes FROM review_hosted_observations WHERE attempt_id=? ORDER BY position DESC LIMIT 1`, a.ID).Scan(&observationBody, &checksum, &captureBytes); err != nil {
		return empty, err
	}
	var observation HostedObservation
	if json.Unmarshal([]byte(observationBody), &observation) != nil || observation.Input.Kind != "" || checksum != request.CaptureSHA256 || observation.Input.CaptureID != release.CaptureID || input.CaptureSHA256 != checksum || input.CaptureBytes != captureBytes || input.ExpectedAnchor != *a.Anchor {
		return empty, retainedRecoveryUnavailable()
	}
	var occupied bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_execution_evidence WHERE attempt_id=?) OR EXISTS(SELECT 1 FROM review_execution_reservations WHERE repository_id=? AND pull_number=? AND action_class='hosted') OR EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND pull_number=? AND channel='hosted' AND id!=? AND state IN ('queued','preparing','posting','active','uncertain'))`, a.ID, a.Anchor.RepositoryID, o.PullNumber, scope.ProjectID, o.PullNumber, o.ID).Scan(&occupied); err != nil {
		return empty, err
	}
	if occupied {
		return empty, retainedRecoveryUnavailable()
	}
	if err = validateReviewEvidence(input); err != nil {
		return empty, err
	}
	if err = validateNewCoverageLimits(input); err != nil {
		return empty, err
	}
	if input.Hosted == nil || validateHostedEvidenceIdentity(a, o, input) != nil {
		return empty, retainedRecoveryUnavailable()
	}
	finished, _ := time.Parse(time.RFC3339Nano, input.FinishedAt)
	if finished.After(terminal) {
		return empty, retainedRecoveryUnavailable()
	}
	credited, err := hasCreditedNativeHostedAlias(ctx, tx, scope.ProjectID, o.PullNumber, input.Hosted)
	if err != nil {
		return empty, err
	}
	if credited {
		return empty, retainedRecoveryUnavailable()
	}
	var epoch, grant int64
	err = tx.QueryRowContext(ctx, `SELECT epoch,grant_revision FROM review_round_operation_bindings WHERE operation_id=? AND project_id=? AND pull_number=? AND channel='hosted'`, o.ID, scope.ProjectID, o.PullNumber).Scan(&epoch, &grant)
	control, controlErr := readRoundRecord(ctx, tx, scope.ProjectID, o.PullNumber, "hosted")
	if controlErr != nil {
		return empty, controlErr
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	if control.Configured {
		if err != nil || control.Epoch != epoch || grant > control.grantRevision || (!control.cumulative && grant != control.grantRevision) || (!control.MaximumUnbounded && control.Used >= control.Maximum) {
			return empty, retainedRecoveryUnavailable()
		}
		occupied, occupancyErr := roundOccupied(ctx, tx, control, o.ID)
		if occupancyErr != nil {
			return empty, occupancyErr
		}
		if !roundHasCapacity(control.RoundControl, occupied) {
			return empty, retainedRecoveryUnavailable()
		}
		original, grantErr := readRoundGrant(ctx, tx, scope.ProjectID, o.PullNumber, "hosted", grant, epoch)
		if grantErr != nil {
			return empty, grantErr
		}
		used, countErr := roundCompletionCount(ctx, tx, scope.ProjectID, o.PullNumber, "hosted", epoch)
		if countErr != nil {
			return empty, countErr
		}
		if control.Used != used || original.Used > used {
			return empty, retainedRecoveryUnavailable()
		}
	} else if err == nil {
		return empty, retainedRecoveryUnavailable()
	}
	canonical, err := json.Marshal(input)
	if err != nil || len(canonical) > evidenceInputLimit {
		return empty, invalid()
	}
	digest := sha256.Sum256(canonical)
	evidence := ReviewEvidence{ID: core.NewID("review_evidence"), ProjectID: scope.ProjectID, AttemptID: a.ID, OperationID: o.ID, CapabilityFingerprint: o.CapabilityFingerprint, Anchor: *a.Anchor, Capture: input, RecordedAt: stamp()}
	body, err := json.Marshal(evidence)
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_evidence(id,project_id,attempt_id,operation_id,body_json,fingerprint) VALUES(?,?,?,?,?,?)`, evidence.ID, scope.ProjectID, a.ID, o.ID, string(body), hex.EncodeToString(digest[:])); err != nil {
		return empty, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.captured", evidence.ID, 1, struct {
		AttemptID   string `json:"attempt_id"`
		OperationID string `json:"operation_id"`
	}{a.ID, o.ID}); err != nil {
		return empty, err
	}
	at, err := attemptRecordingStamp(ctx, tx, o, &a, e.sampleAttemptTime())
	if err != nil {
		return empty, err
	}
	const reason = "hosted_retained_evidence_recovered"
	if _, err = tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state='completed',revision=revision+1 WHERE id=?`, a.ID); err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE review_operations SET state='completed',revision=revision+1,reason=?,updated_at=? WHERE id=?`, reason, at, o.ID); err != nil {
		return empty, err
	}
	a.State = "completed"
	a.Revision++
	if e.publishAttempt != nil {
		if _, err = publishEvidenceTx(ctx, tx, scope.ProjectID, evidence.ID, reviews.PullInput{}, true); err != nil {
			return empty, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_execution_transitions(attempt_id,revision,state,reason,external_id,created_at,source_refusal_json) VALUES(?,?,?,?,?,?,?)`, a.ID, a.Revision, a.State, reason, a.ExternalID, at, sourceRefusalJSON(nil)); err != nil {
		return empty, err
	}
	if e.publishAttempt != nil {
		if _, err = publishJournalAttempt(ctx, tx, a, nil, false, true); err != nil {
			return empty, err
		}
	}
	if err = refreshComparisonProof(ctx, tx, scope.ProjectID, o.ID); err != nil {
		return empty, err
	}
	if err = completeRoundControl(ctx, tx, scope.ProjectID, o.ID); err != nil {
		return empty, err
	}
	if err = e.publishCompletionNotice(ctx, tx, o, a.Provider); err != nil {
		return empty, err
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.operation_updated", o.ID, o.Revision+1, struct{}{}); err != nil {
		return empty, err
	}
	receipt := hostedRetainedReceipt{a.ID, o.ID, evidence.ID, a.Revision, a.State}
	if err = core.AppendEvent(ctx, tx, scope, "review.hosted_retained_recovered", a.ID, a.Revision, struct {
		Request hostedRetainedRequest `json:"request"`
		Receipt hostedRetainedReceipt `json:"receipt"`
	}{request, receipt}); err != nil {
		return empty, err
	}
	return receipt, nil
}
