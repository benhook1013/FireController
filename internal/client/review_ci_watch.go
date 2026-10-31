package client

import (
	"context"
	"encoding/json"
	"net/http"
)

const CIWatchMaxTargets = 20
const CIWatchDefaultSeconds = 300

type CIWatchTarget struct {
	PullNumber int64  `json:"pull_number"`
	HeadSHA    string `json:"head_sha"`
	BaseSHA    string `json:"base_sha,omitempty"`
}

type CIWatchTargetResult struct {
	PullNumber      int64             `json:"pull_number"`
	HeadSHA         string            `json:"head_sha"`
	BaseSHA         string            `json:"base_sha,omitempty"`
	State           string            `json:"state"`
	Reason          string            `json:"reason"`
	BaseRef         string            `json:"base_ref,omitempty"`
	GateRef         string            `json:"gate_ref,omitempty"`
	ObservedHeadSHA string            `json:"observed_head_sha,omitempty"`
	ObservedBaseSHA string            `json:"observed_base_sha,omitempty"`
	ObservedAt      string            `json:"observed_at,omitempty"`
	CICommitSHA     string            `json:"ci_commit_sha,omitempty"`
	Selection       string            `json:"selection,omitempty"`
	Required        int64             `json:"required"`
	Satisfied       int64             `json:"satisfied"`
	Failed          int64             `json:"failed"`
	Pending         int64             `json:"pending"`
	Unproven        int64             `json:"unproven"`
	Checks          []ReviewCIContext `json:"checks"`
}

type CIWatchResult struct {
	ProjectID string                `json:"project_id"`
	State     string                `json:"state"`
	TimedOut  bool                  `json:"timed_out"`
	Targets   []CIWatchTargetResult `json:"targets"`
}

func ValidateCIWatchTargets(targets []CIWatchTarget) error {
	if len(targets) < 1 || len(targets) > CIWatchMaxTargets {
		return validation()
	}
	seen := map[int64]bool{}
	for _, target := range targets {
		if !validPullNumber(target.PullNumber) || seen[target.PullNumber] || !controlHex(target.HeadSHA, 40) || target.BaseSHA != "" && !controlHex(target.BaseSHA, 40) {
			return validation()
		}
		seen[target.PullNumber] = true
	}
	return nil
}

// WatchCI collects fresh CI facts for exact commit pins. This POST is a read:
// it has no idempotency key, enrollment, mutation retry or unknown-write result.
func (c *Client) WatchCI(ctx context.Context, targets []CIWatchTarget, seconds int) (CIWatchResult, error) {
	if ValidateCIWatchTargets(targets) != nil || ValidateReviewOperationWait(seconds) != nil {
		return CIWatchResult{}, validation()
	}
	body, err := json.Marshal(struct {
		Targets []CIWatchTarget `json:"targets"`
	}{targets})
	if err != nil {
		return CIWatchResult{}, validation()
	}
	var value CIWatchResult
	var status int
	if err := c.requestBoundedOptions(ctx, http.MethodPost, c.projectPath("reviews", "ci", "watch"), nil, true, "", body, http.StatusOK, false, &value, 512<<10, decodeReviewResponse, &sourceWaitOptions{seconds: seconds, status: &status}); err != nil {
		return CIWatchResult{}, err
	}
	if value.ProjectID != c.identity.ProjectID || !validCIWatchResult(value, targets) {
		return CIWatchResult{}, invalidResponse()
	}
	return value, nil
}

func validCIWatchResult(value CIWatchResult, targets []CIWatchTarget) bool {
	if len(value.Targets) != len(targets) {
		return false
	}
	state := "pending"
	for i, item := range value.Targets {
		pin := targets[i]
		if item.PullNumber != pin.PullNumber || item.HeadSHA != pin.HeadSHA || item.BaseSHA != pin.BaseSHA || !oneReviewOf(item.State, "pending", "passed", "failed", "superseded", "unconfigured", "closed") || item.Reason != "" && !validExecutionIdentifier(item.Reason) || !controlText(item.BaseRef, 256, false) || !controlText(item.GateRef, 256, false) || item.ObservedAt != "" && !controlTime(item.ObservedAt) || item.ObservedHeadSHA != "" && !controlHex(item.ObservedHeadSHA, 40) || item.ObservedBaseSHA != "" && !controlHex(item.ObservedBaseSHA, 40) {
			return false
		}
		if item.Selection == "" && item.CICommitSHA != "" || item.Selection != "" && (!oneReviewOf(item.Selection, "head", "test_merge") || !controlHex(item.CICommitSHA, 40)) {
			return false
		}
		for _, count := range []int64{item.Required, item.Satisfied, item.Failed, item.Pending, item.Unproven} {
			if count < 0 || count > 128 {
				return false
			}
		}
		if item.Required != item.Satisfied+item.Failed+item.Pending+item.Unproven || !validCIContextDetails(ReviewSourceCIAssessment{Checks: &item.Checks, Required: item.Required, Satisfied: item.Satisfied, Failed: item.Failed, Pending: item.Pending, Unproven: item.Unproven}) {
			return false
		}
		if item.State == "passed" && (item.Required == 0 || item.Satisfied != item.Required) || item.State == "unconfigured" && item.Required != 0 || item.State == "failed" && item.Failed == 0 {
			return false
		}
		if oneReviewOf(item.State, "passed", "failed", "unconfigured") && (item.ObservedHeadSHA != pin.HeadSHA || item.ObservedBaseSHA == "" || pin.BaseSHA != "" && item.ObservedBaseSHA != pin.BaseSHA || item.BaseRef == "" || item.GateRef == "" || item.ObservedAt == "") {
			return false
		}
		if item.State == "passed" && (item.Selection == "" || item.Selection == "head" && item.CICommitSHA != pin.HeadSHA) {
			return false
		}
		if state == "pending" && item.State != "pending" {
			state = item.State
		}
	}
	return value.State == state && (state == "pending" || !value.TimedOut)
}
