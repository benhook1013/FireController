package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const reviewBatchMaxBytes = 2 << 20

type FindingDecisionBatchItem struct {
	Finding               *int64 `json:"finding"`
	Disposition           string `json:"disposition"`
	Reason                string `json:"reason"`
	ExpectedRouteRevision int64  `json:"expected_route_revision,omitempty"`
}
type FindingDecisionBatchRequest struct {
	ExpectedRevision *int64                     `json:"expected_revision"`
	Items            []FindingDecisionBatchItem `json:"items"`
}

// Omission means the existing zero route guard; explicit null is never a guard.
func (item *FindingDecisionBatchItem) UnmarshalJSON(data []byte) error {
	var wire struct {
		Finding               *int64 `json:"finding"`
		Disposition           string `json:"disposition"`
		Reason                string `json:"reason"`
		ExpectedRouteRevision *int64 `json:"expected_route_revision,omitempty"`
	}
	wire.ExpectedRouteRevision = new(int64)
	if strictjson.Decode(data, &wire) != nil || wire.ExpectedRouteRevision == nil {
		return invalid()
	}
	*item = FindingDecisionBatchItem{Finding: wire.Finding, Disposition: wire.Disposition, Reason: wire.Reason, ExpectedRouteRevision: *wire.ExpectedRouteRevision}
	return nil
}

type FindingFixBatchItem struct {
	Finding     *int64  `json:"finding"`
	ExpectedSHA *string `json:"expected_sha"`
	FixSHA      string  `json:"fix_sha"`
	ProofNote   string  `json:"proof_note"`
}
type FindingFixBatchRequest struct {
	ExpectedRevision *int64                `json:"expected_revision"`
	Items            []FindingFixBatchItem `json:"items"`
}

// Each batch owns exactly one mutation transaction and one adjudication revision.
// Item guards run against that shared state; an error rolls back route effects too.
func (e *Engine) DecideReviewFindings(ctx context.Context, scope core.Scope, key, id string, request FindingDecisionBatchRequest) (json.RawMessage, error) {
	if !identifier.MatchString(id) || !validAdjudicationRevision(request.ExpectedRevision) || len(request.Items) < 1 || len(request.Items) > 100 {
		return nil, invalid()
	}
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	request.Items = append([]FindingDecisionBatchItem(nil), request.Items...)
	seen := map[int64]bool{}
	for i, item := range request.Items {
		if item.Finding == nil || seen[*item.Finding] || !validFindingDecisionAction(FindingDecisionRequest{Finding: *item.Finding, Disposition: item.Disposition, Reason: item.Reason, ExpectedRouteRevision: item.ExpectedRouteRevision}) {
			return nil, invalid()
		}
		finding := *item.Finding
		seen[finding] = true
		request.Items[i].Finding = &finding
	}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.decide_batch", Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		evidence, state, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, conflict()
		}
		var at string
		for _, item := range request.Items {
			var itemAt string
			itemAt, err = e.applyFindingDecision(ctx, tx, scope, evidence, &state, FindingDecisionRequest{Finding: *item.Finding, Disposition: item.Disposition, Reason: item.Reason, ExpectedRouteRevision: item.ExpectedRouteRevision})
			if err != nil {
				return nil, err
			}
			at = routeStampAtLeast(itemAt, at)
		}
		return e.persistReviewAdjudication(ctx, tx, scope, state, "adjudicated_batch", -1, request, at)
	})
}

func (e *Engine) RecordFindingFixes(ctx context.Context, scope core.Scope, key, id string, request FindingFixBatchRequest) (json.RawMessage, error) {
	if !identifier.MatchString(id) || !validAdjudicationRevision(request.ExpectedRevision) || len(request.Items) < 1 || len(request.Items) > 100 {
		return nil, invalid()
	}
	expected := *request.ExpectedRevision
	request.ExpectedRevision = &expected
	request.Items = append([]FindingFixBatchItem(nil), request.Items...)
	seen := map[int64]bool{}
	for i, item := range request.Items {
		if item.Finding == nil || seen[*item.Finding] || !validFindingFixAction(FindingFixRequest{Finding: *item.Finding, ExpectedSHA: item.ExpectedSHA, FixSHA: item.FixSHA, ProofNote: item.ProofNote}) {
			return nil, invalid()
		}
		finding, prior := *item.Finding, *item.ExpectedSHA
		seen[finding] = true
		request.Items[i].Finding, request.Items[i].ExpectedSHA = &finding, &prior
	}
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.evidence.fix_batch", Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		_, state, err := loadReviewAdjudication(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if !adjudicationMatches(state, expected) {
			return nil, conflict()
		}
		var at string
		for _, item := range request.Items {
			at, err = applyFindingFix(&state, FindingFixRequest{Finding: *item.Finding, ExpectedSHA: item.ExpectedSHA, FixSHA: item.FixSHA, ProofNote: item.ProofNote})
			if err != nil {
				return nil, err
			}
		}
		return e.persistReviewAdjudication(ctx, tx, scope, state, "fixes_recorded_batch", -1, request, at)
	})
}

func (e *Engine) decideEvidenceBatchHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var request FindingDecisionBatchRequest
	if err := core.ReadJSONLimit(w, r, &request, reviewBatchMaxBytes); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := e.DecideReviewFindings(r.Context(), scope, key, id, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, result)
}
func (e *Engine) fixEvidenceBatchHTTP(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := e.evidenceHTTPScope(w, r, "write")
	if !ok {
		return
	}
	var request FindingFixBatchRequest
	if err := core.ReadJSONLimit(w, r, &request, reviewBatchMaxBytes); err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := e.RecordFindingFixes(r.Context(), scope, key, id, request)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, result)
}
