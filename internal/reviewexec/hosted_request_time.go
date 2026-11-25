package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

type hostedRequestTimeReceipt struct {
	RunID       string `json:"run_id"`
	RequestedAt string `json:"requested_at"`
}

func (e *Engine) recordHostedRequestTimeHTTP(w http.ResponseWriter, r *http.Request, scope core.Scope, id, key string, request hostedRetainedRequest) {
	mutation := core.Mutation{Operation: "reviews.hosted.request-time", Target: id, Key: key, Request: request}
	ctx := r.Context()
	receipt, found, err := e.store.Replay(ctx, scope, mutation)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if !found {
		if e.hostedRetainedDecoder == nil {
			core.WriteError(w, retainedRecoveryUnavailable())
			return
		}
		input, err := e.hostedRetainedDecoder(ctx, e, scope.ProjectID, id, request.CapabilityID, request.CaptureSHA256)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		receipt, err = e.store.Mutate(ctx, scope, mutation, func(ctx context.Context, tx *sql.Tx) (any, error) {
			return e.recordHostedRequestTime(ctx, tx, scope, id, request, input)
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
	}
	core.WriteJSON(w, http.StatusOK, json.RawMessage(receipt))
}

func (e *Engine) recordHostedRequestTime(ctx context.Context, tx *sql.Tx, scope core.Scope, id string, request hostedRetainedRequest, input ReviewEvidenceInput) (hostedRequestTimeReceipt, error) {
	empty := hostedRequestTimeReceipt{}
	attempt, operation, err := readHostedPostingAttempt(ctx, tx, scope.ProjectID, id)
	if err != nil {
		return empty, err
	}
	if attempt.State != "failed" || operation.State != "failed" || operation.Reason != HostedFinishedIncompleteReason ||
		attempt.Anchor == nil || input.ExpectedAnchor != *attempt.Anchor || input.CaptureSHA256 != request.CaptureSHA256 {
		return empty, retainedRecoveryUnavailable()
	}
	guard, err := hostedRecoveryTarget(ctx, tx, operation, SettlementTarget{
		AttemptRevision: attempt.Revision, RepositoryID: attempt.Anchor.RepositoryID,
		HeadSHA: attempt.Anchor.CandidateSHA, ExternalID: attempt.ExternalID,
	})
	if err != nil {
		return empty, err
	}
	if guard == nil || *guard != request || validateReviewEvidence(input) != nil ||
		input.Hosted == nil || validateHostedEvidenceIdentity(attempt, operation, input) != nil {
		return empty, retainedRecoveryUnavailable()
	}
	run, err := reviews.RecordExternalHostedRequestTime(ctx, tx, scope.ProjectID, request.RepositoryID,
		operation.PullNumber, request.HeadSHA, nativeHostedExternalSourceID(input.Hosted), input.StartedAt, input.FinishedAt)
	if err != nil {
		return empty, err
	}
	if err := core.AppendEvent(ctx, tx, scope, "review.hosted_request_time_recorded", run, 1, struct{}{}); err != nil {
		return empty, err
	}
	return hostedRequestTimeReceipt{RunID: run, RequestedAt: input.StartedAt}, nil
}
