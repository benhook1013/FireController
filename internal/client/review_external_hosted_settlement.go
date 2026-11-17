package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

const ExternalHostedSettlementMaxBytes = 2 << 20

type ExternalHostedSettlementRequest struct {
	CountRound bool                          `json:"count_round"`
	Runs       []ExternalHostedSettlementRun `json:"runs"`
}

type ExternalHostedSettlementRun struct {
	RunID    string                            `json:"run_id"`
	Findings []ExternalHostedSettlementFinding `json:"findings"`
}

type ExternalHostedSettlementFinding struct {
	FindingID   string `json:"finding_id"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
	FixSHA      string `json:"fix_sha,omitempty"`
	ProofNote   string `json:"proof_note,omitempty"`
}

type ExternalHostedSettlementReceipt struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Runs         int    `json:"runs"`
	RoundCounted bool   `json:"round_counted"`
}

// DecodeExternalHostedSettlementRequest requires an explicit count_round and
// findings array on each run, including false and an empty array respectively.
func DecodeExternalHostedSettlementRequest(data []byte) (ExternalHostedSettlementRequest, error) {
	var value ExternalHostedSettlementRequest
	if len(data) > ExternalHostedSettlementMaxBytes || decodeResponse(data, &value) != nil {
		return ExternalHostedSettlementRequest{}, mutationValidation()
	}
	if err := validateExternalHostedSettlement(value); err != nil {
		return ExternalHostedSettlementRequest{}, err
	}
	return value, nil
}

func validateExternalHostedSettlement(value ExternalHostedSettlementRequest) error {
	if len(value.Runs) < 1 || len(value.Runs) > 10 {
		return mutationValidation()
	}
	runIDs := make(map[string]bool, len(value.Runs))
	total := 0
	for i, run := range value.Runs {
		total += len(run.Findings)
		if !validReviewRunID(run.RunID) || runIDs[run.RunID] || run.Findings == nil || total > 100 {
			return mutationValidation()
		}
		runIDs[run.RunID] = true
		findingIDs := make(map[string]bool, len(run.Findings))
		for j, finding := range run.Findings {
			if !controlText(finding.FindingID, 256, true) || findingIDs[finding.FindingID] || !controlText(finding.Reason, 2048, true) {
				return mutationValidation()
			}
			findingIDs[finding.FindingID] = true
			switch finding.Disposition {
			case "accepted":
				if err := reviewFixSHAValidation(fmt.Sprintf("runs[%d].findings[%d].", i, j), "", finding.FixSHA); err != nil {
					return err
				}
				if !controlText(finding.ProofNote, 2048, true) {
					return mutationValidation()
				}
			case "rejected":
				if finding.FixSHA != "" || finding.ProofNote != "" {
					return mutationValidation()
				}
			default:
				return mutationValidation()
			}
		}
	}
	return nil
}

func (c *Client) SettleExternalHosted(ctx context.Context, key string, pullNumber int64, value ExternalHostedSettlementRequest) (ExternalHostedSettlementReceipt, error) {
	var receipt ExternalHostedSettlementReceipt
	if !validMutationKey(key) || !validPullNumber(pullNumber) {
		return receipt, mutationValidation()
	}
	if err := validateExternalHostedSettlement(value); err != nil {
		return receipt, err
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	// This body is JSON sent over HTTP, so HTML escaping would only inflate
	// accepted reason/proof text and could exceed the server's byte limit.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil || body.Len() > ExternalHostedSettlementMaxBytes {
		return receipt, mutationValidation()
	}
	path := c.projectPath("reviews", "external-hosted", strconv.FormatInt(pullNumber, 10), "settlement")
	if err := c.request(ctx, http.MethodPost, path, nil, true, key, body.Bytes(), http.StatusOK, true, &receipt); err != nil {
		return ExternalHostedSettlementReceipt{}, err
	}
	if !validReviewOpaqueID(receipt.ID, "review_hosted_batch_") || receipt.State != "external_hosted_settled" || receipt.Runs != len(value.Runs) || receipt.RoundCounted != value.CountRound {
		return ExternalHostedSettlementReceipt{}, unknownMutation("invalid_response", "The service returned an invalid Hosted settlement receipt; its outcome is unknown.", http.StatusOK)
	}
	return receipt, nil
}

func externalHostedPendingDiagnostic() *ReviewOperationDiagnostic {
	return &ReviewOperationDiagnostic{
		Explanation: "Manual Hosted results on this PR still need settlement.",
		NextAction:  "Read reviews runs PR and reviews run RUN_ID (MCP review_runs and review_run) for the retained results. Decide the findings, verify accepted fixes, then use reviews settle-hosted (MCP review_external_hosted_settle) before requesting another Hosted review.",
	}
}

func safeExternalHostedPendingDetails(value any) any {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 3 {
		return nil
	}
	pull, pullOK := object["pull_number"].(float64)
	ids, idsOK := object["run_ids"].([]any)
	more, moreOK := object["has_more"].(bool)
	if !pullOK || pull < 1 || pull > 2147483647 || pull != float64(int64(pull)) ||
		!idsOK || len(ids) < 1 || len(ids) > 10 || !moreOK {
		return nil
	}
	runIDs := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, raw := range ids {
		id, ok := raw.(string)
		if !ok || !validReviewRunID(id) || seen[id] {
			return nil
		}
		seen[id] = true
		runIDs = append(runIDs, id)
	}
	return map[string]any{"pull_number": int64(pull), "run_ids": runIDs, "has_more": more}
}
