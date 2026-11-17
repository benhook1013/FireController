package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// This accommodates 100 explicitly selected 2048-byte reasons/proofs even after
// JSON escaping. The bound is shared by file, MCP and HTTP request admission.
const ReviewBatchMaxBytes = 2 << 20

type ReviewDecisionBatchItem struct {
	Finding               int64  `json:"finding"`
	Disposition           string `json:"disposition"`
	Reason                string `json:"reason"`
	ExpectedRouteRevision int64  `json:"expected_route_revision,omitempty"`
}
type ReviewDecisionBatchRequest struct {
	ExpectedRevision int64                     `json:"expected_revision"`
	Items            []ReviewDecisionBatchItem `json:"items"`
}
type ReviewFixBatchItem struct {
	Finding     int64  `json:"finding"`
	ExpectedSHA string `json:"expected_sha"`
	FixSHA      string `json:"fix_sha"`
	ProofNote   string `json:"proof_note"`
}
type ReviewFixBatchRequest struct {
	ExpectedRevision int64                `json:"expected_revision"`
	Items            []ReviewFixBatchItem `json:"items"`
}

func ValidateReviewDecisionBatchRequest(r ReviewDecisionBatchRequest) error {
	if !validControlRevision(r.ExpectedRevision) || len(r.Items) < 1 || len(r.Items) > 100 {
		return mutationValidation()
	}
	seen := map[int64]bool{}
	for _, item := range r.Items {
		if seen[item.Finding] || ValidateReviewDecisionRequest(ReviewDecisionRequest{ExpectedRevision: r.ExpectedRevision, Finding: item.Finding, Disposition: item.Disposition, Reason: item.Reason, ExpectedRouteRevision: item.ExpectedRouteRevision}) != nil {
			return mutationValidation()
		}
		seen[item.Finding] = true
	}
	return nil
}
func ValidateReviewFixBatchRequest(r ReviewFixBatchRequest) error {
	if !validControlRevision(r.ExpectedRevision) || len(r.Items) < 1 || len(r.Items) > 100 {
		return mutationValidation()
	}
	seen := map[int64]bool{}
	for i, item := range r.Items {
		if err := reviewFixSHAValidation(fmt.Sprintf("items[%d].", i), item.ExpectedSHA, item.FixSHA); err != nil {
			return err
		}
		if seen[item.Finding] || ValidateReviewFixRequest(ReviewFixRequest{ExpectedRevision: r.ExpectedRevision, Finding: item.Finding, ExpectedSHA: item.ExpectedSHA, FixSHA: item.FixSHA, ProofNote: item.ProofNote}) != nil {
			return mutationValidation()
		}
		seen[item.Finding] = true
	}
	return nil
}

// Decode validates explicit required fields, including finding zero and an empty
// expected SHA. Typed methods always serialize these fields themselves.
func DecodeReviewDecisionBatchRequest(data []byte) (ReviewDecisionBatchRequest, error) {
	var wire struct {
		ExpectedRevision *int64 `json:"expected_revision"`
		Items            []struct {
			Finding               *int64          `json:"finding"`
			Disposition           *string         `json:"disposition"`
			Reason                *string         `json:"reason"`
			ExpectedRouteRevision json.RawMessage `json:"expected_route_revision,omitempty"`
		} `json:"items"`
	}
	if len(data) > ReviewBatchMaxBytes || strictjson.Decode(data, &wire) != nil || wire.ExpectedRevision == nil {
		return ReviewDecisionBatchRequest{}, mutationValidation()
	}
	r := ReviewDecisionBatchRequest{ExpectedRevision: *wire.ExpectedRevision, Items: make([]ReviewDecisionBatchItem, len(wire.Items))}
	for i, item := range wire.Items {
		if item.Finding == nil || item.Disposition == nil || item.Reason == nil {
			return ReviewDecisionBatchRequest{}, mutationValidation()
		}
		var route int64
		if len(item.ExpectedRouteRevision) > 0 && (bytes.Equal(bytes.TrimSpace(item.ExpectedRouteRevision), []byte("null")) || strictjson.Decode(item.ExpectedRouteRevision, &route) != nil) {
			return ReviewDecisionBatchRequest{}, mutationValidation()
		}
		r.Items[i] = ReviewDecisionBatchItem{Finding: *item.Finding, Disposition: *item.Disposition, Reason: *item.Reason, ExpectedRouteRevision: route}
	}
	return r, ValidateReviewDecisionBatchRequest(r)
}
func DecodeReviewFixBatchRequest(data []byte) (ReviewFixBatchRequest, error) {
	var wire struct {
		ExpectedRevision *int64 `json:"expected_revision"`
		Items            []struct {
			Finding     *int64  `json:"finding"`
			ExpectedSHA *string `json:"expected_sha"`
			FixSHA      *string `json:"fix_sha"`
			ProofNote   *string `json:"proof_note"`
		} `json:"items"`
	}
	if len(data) > ReviewBatchMaxBytes || strictjson.Decode(data, &wire) != nil || wire.ExpectedRevision == nil {
		return ReviewFixBatchRequest{}, mutationValidation()
	}
	r := ReviewFixBatchRequest{ExpectedRevision: *wire.ExpectedRevision, Items: make([]ReviewFixBatchItem, len(wire.Items))}
	for i, item := range wire.Items {
		if item.Finding == nil || item.ExpectedSHA == nil || item.FixSHA == nil || item.ProofNote == nil {
			return ReviewFixBatchRequest{}, mutationValidation()
		}
		r.Items[i] = ReviewFixBatchItem{Finding: *item.Finding, ExpectedSHA: *item.ExpectedSHA, FixSHA: *item.FixSHA, ProofNote: *item.ProofNote}
	}
	return r, ValidateReviewFixBatchRequest(r)
}
func (c *Client) DecideReviewFindings(ctx context.Context, key, id string, r ReviewDecisionBatchRequest) (ReviewControlReceipt, error) {
	if err := ValidateReviewDecisionBatchRequest(r); err != nil {
		return ReviewControlReceipt{}, err
	}
	return c.reviewBatchControl(ctx, key, id, "decisions", "adjudicated_batch", r.ExpectedRevision, r)
}
func (c *Client) RecordReviewFixes(ctx context.Context, key, id string, r ReviewFixBatchRequest) (ReviewControlReceipt, error) {
	if err := ValidateReviewFixBatchRequest(r); err != nil {
		return ReviewControlReceipt{}, err
	}
	return c.reviewBatchControl(ctx, key, id, "fixes", "fixes_recorded_batch", r.ExpectedRevision, r)
}

func (c *Client) reviewBatchControl(ctx context.Context, key, id, suffix, state string, expected int64, r any) (ReviewControlReceipt, error) {
	if !validMutationKey(key) || ValidateReviewEvidenceID(id) != nil {
		return ReviewControlReceipt{}, mutationValidation()
	}
	var receipt ReviewControlReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "evidence", id, suffix, "batch"), key, r, http.StatusOK, &receipt); err != nil {
		return ReviewControlReceipt{}, err
	}
	if receipt.ID != id || receipt.State != state || receipt.Revision != expected+1 {
		return ReviewControlReceipt{}, unknownMutation("invalid_response", "The service returned an invalid review receipt; its outcome is unknown.", http.StatusOK)
	}
	return receipt, nil
}
