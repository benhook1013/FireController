package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const CIWatchMaxTargets = 20
const CIWatchDefaultSeconds = 300

type CIWatchTarget struct {
	PullNumber          int64   `json:"pull_number"`
	HeadSHA             string  `json:"head_sha"`
	BaseSHA             string  `json:"base_sha,omitempty"`
	AfterObservationKey string  `json:"after_observation_key,omitempty"`
	FailureWorkflowIDs  []int64 `json:"failure_workflow_ids,omitempty"`
}

type CIWorkflowFailure struct {
	WorkflowID int64  `json:"workflow_id"`
	RunID      int64  `json:"run_id"`
	RunAttempt int64  `json:"run_attempt"`
	JobID      int64  `json:"job_id"`
	CheckRunID int64  `json:"check_run_id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

type CIWatchTargetResult struct {
	PullNumber           int64                   `json:"pull_number"`
	HeadSHA              string                  `json:"head_sha"`
	BaseSHA              string                  `json:"base_sha,omitempty"`
	State                string                  `json:"state"`
	Reason               string                  `json:"reason"`
	BaseRef              string                  `json:"base_ref,omitempty"`
	GateRef              string                  `json:"gate_ref,omitempty"`
	ObservedHeadSHA      string                  `json:"observed_head_sha,omitempty"`
	ObservedBaseSHA      string                  `json:"observed_base_sha,omitempty"`
	ObservedAt           string                  `json:"observed_at,omitempty"`
	CICommitSHA          string                  `json:"ci_commit_sha,omitempty"`
	Selection            string                  `json:"selection,omitempty"`
	Required             int64                   `json:"required"`
	Satisfied            int64                   `json:"satisfied"`
	Failed               int64                   `json:"failed"`
	Pending              int64                   `json:"pending"`
	Unproven             int64                   `json:"unproven"`
	Checks               []ReviewCIContext       `json:"checks"`
	ObservationKey       string                  `json:"observation_key,omitempty"`
	WaitingForChange     bool                    `json:"waiting_for_change,omitempty"`
	WorkflowFailures     []CIWorkflowFailure     `json:"workflow_failures,omitempty"`
	WorkflowObservations []CIWorkflowObservation `json:"workflow_observations,omitempty"`
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
		if !validPullNumber(target.PullNumber) || seen[target.PullNumber] || !controlHex(target.HeadSHA, 40) || target.BaseSHA != "" && !controlHex(target.BaseSHA, 40) || target.AfterObservationKey != "" && !controlHex(target.AfterObservationKey, 64) {
			return validation()
		}
		seen[target.PullNumber] = true
		if target.FailureWorkflowIDs != nil && len(target.FailureWorkflowIDs) == 0 || len(target.FailureWorkflowIDs) > 16 {
			return validation()
		}
		workflows := map[int64]bool{}
		for _, id := range target.FailureWorkflowIDs {
			if id <= 0 || workflows[id] {
				return validation()
			}
			workflows[id] = true
		}
	}
	return nil
}

// WatchCI collects fresh CI facts for exact commit pins. This POST is a read:
// it has no idempotency key, enrollment, mutation retry or unknown-write result.
func (c *Client) WatchCI(ctx context.Context, targets []CIWatchTarget, seconds int) (CIWatchResult, error) {
	if ValidateCIWatchTargets(targets) != nil || ValidateReviewOperationWait(seconds) != nil {
		return CIWatchResult{}, validation()
	}
	targets = append([]CIWatchTarget(nil), targets...)
	for i := range targets {
		targets[i].FailureWorkflowIDs = append([]int64(nil), targets[i].FailureWorkflowIDs...)
	}
	body, err := json.Marshal(struct {
		Targets []CIWatchTarget `json:"targets"`
	}{targets})
	if err != nil {
		return CIWatchResult{}, validation()
	}
	// Client setup shares the caller deadline. The requested wait is an upper
	// bound; reserve the existing response margin before starting this read.
	if deadline, ok := ctx.Deadline(); ok && seconds > 0 {
		available := max(0, time.Until(deadline)-5*time.Second) / time.Second
		seconds = min(seconds, int(available))
	}
	workflowDetails := false
	for _, target := range targets {
		workflowDetails = workflowDetails || len(target.FailureWorkflowIDs) > 0
	}
	var value CIWatchResult
	var status int
	err = resumeResultWait(ctx, seconds, func(ctx context.Context, remaining int) error {
		value = CIWatchResult{}
		return c.requestBoundedOptions(ctx, http.MethodPost, c.projectPath("reviews", "ci", "watch"), nil, true, "", body, http.StatusOK, false, &value, 512<<10, decodeReviewResponse, &sourceWaitOptions{seconds: remaining, status: &status, ciWatchChange: true, ciWorkflowDetails: workflowDetails})
	})
	if err != nil {
		return CIWatchResult{}, reviewWaitTargetError(ciWatchReadError(err), "", targets)
	}
	if value.ProjectID != c.identity.ProjectID || !validCIWatchResult(value, targets) {
		return CIWatchResult{}, invalidResponse()
	}
	return value, nil
}

// Only local interruption errors receive this read-specific explanation. A
// service refusal or connection failure keeps its existing diagnostic.
func ciWatchReadError(err error) error {
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 0 || !oneReviewOf(failure.Code, "canceled", "deadline_exceeded") {
		return err
	}
	result := *failure
	result.Message = "The CI wait was interrupted because the client context was canceled; no CI result was received."
	if failure.Code == "deadline_exceeded" {
		result.Message = "The client CI wait deadline expired before a CI result was received."
	}
	result.Action = "No review or CI rerun was requested. This does not establish CI failure or a changed PR head. Start another bounded CI wait with the same PR/head/base pins only when needed; use different pins only after independently verifying the comparison."
	return &result
}

func validCIWatchResult(value CIWatchResult, targets []CIWatchTarget) bool {
	if len(value.Targets) != len(targets) {
		return false
	}
	state := "pending"
	for i, item := range value.Targets {
		pin := targets[i]
		if !validCIWatchWorkflowFailures(item, pin) || !validCIWatchWorkflowObservations(item, pin) {
			return false
		}
		attention := item.State == "pending" && len(item.WorkflowFailures) > 0
		knownFailure := item.State == "failed" || attention
		if item.ObservationKey != "" && !controlHex(item.ObservationKey, 64) || item.WaitingForChange && (!knownFailure || pin.AfterObservationKey == "" || item.ObservationKey != pin.AfterObservationKey) || knownFailure && pin.AfterObservationKey != "" && item.ObservationKey == pin.AfterObservationKey && !item.WaitingForChange {
			return false
		}
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
		if state == "pending" && !item.WaitingForChange {
			if attention {
				state = "attention"
			} else if item.State != "pending" {
				state = item.State
			}
		}
	}
	return value.State == state && (state == "pending" || !value.TimedOut)
}

func validCIWatchWorkflowFailures(item CIWatchTargetResult, pin CIWatchTarget) bool {
	if len(item.WorkflowFailures) == 0 {
		return true
	}
	if item.State != "pending" || len(item.WorkflowFailures) > 16 || item.ObservationKey == "" || item.ObservedHeadSHA != pin.HeadSHA || item.ObservedBaseSHA == "" || pin.BaseSHA != "" && item.ObservedBaseSHA != pin.BaseSHA || item.BaseRef == "" || item.GateRef == "" || item.ObservedAt == "" || item.Selection == "" || item.Selection == "head" && item.CICommitSHA != pin.HeadSHA || item.Reason == "ci_watch_refresh_pending" {
		return false
	}
	selected, workflows, jobs, checks := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, id := range pin.FailureWorkflowIDs {
		selected[id] = true
	}
	for _, failure := range item.WorkflowFailures {
		if !selected[failure.WorkflowID] || workflows[failure.WorkflowID] || failure.RunID <= 0 || failure.RunAttempt <= 0 || failure.JobID <= 0 || jobs[failure.JobID] || failure.CheckRunID <= 0 || checks[failure.CheckRunID] || !controlText(failure.Name, 256, true) || !oneReviewOf(failure.Conclusion, "failure", "timed_out", "action_required", "startup_failure") {
			return false
		}
		workflows[failure.WorkflowID], jobs[failure.JobID], checks[failure.CheckRunID] = true, true, true
	}
	return true
}
