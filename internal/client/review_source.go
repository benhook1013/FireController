package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type ReviewSourceRequest struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	HeadSHA                       string `json:"head_sha"`
	IncludeCI                     bool   `json:"include_ci,omitempty"`
}

// ReviewSourceResult contains exactly one response. JSON stays flat so agents
// receive either terminal facts or the compact fallback receipt.
type ReviewSourceResult struct {
	Receipt   *ReviewRequestReceipt
	Operation *ReviewSourceOperation
}

func (r ReviewSourceResult) MarshalJSON() ([]byte, error) {
	if (r.Receipt == nil) == (r.Operation == nil) {
		return nil, errors.New("invalid source refresh result")
	}
	if r.Operation != nil {
		return json.Marshal(r.Operation)
	}
	return json.Marshal(r.Receipt)
}

func ValidateReviewSourceWait(seconds int) error {
	if seconds < 0 || seconds > 60 {
		return mutationValidation()
	}
	return nil
}

// Only a source-refresh POST can report a known refusal after its enqueue
// committed. Preserve that explicit uncertainty without changing read errors
// or ordinary pre-enqueue authorization refusals.
func sourceResponseOutcomeUnknown(data []byte) bool {
	var value struct {
		Error struct {
			Details struct {
				OutcomeUnknown bool `json:"outcome_unknown"`
			} `json:"details"`
		} `json:"error"`
	}
	return json.Unmarshal(data, &value) == nil && value.Error.Details.OutcomeUnknown
}

// ReviewSourceObservation is retained source identity, not live verification.
type ReviewSourceObservation struct {
	RepositoryID     int64  `json:"repository_id"`
	RepositoryName   string `json:"repository_name"`
	PullNumber       int64  `json:"pull_number"`
	HeadSHA          string `json:"head_sha"`
	BaseSHA          string `json:"base_sha"`
	BaseRef          string `json:"base_ref"`
	ParentPullNumber int64  `json:"parent_pull_number"`
	ParentSHA        string `json:"parent_sha"`
	ObservedAt       string `json:"observed_at"`
}

type ReviewSourceOperation struct {
	ID                    string                   `json:"id"`
	ProjectID             string                   `json:"project_id"`
	PullNumber            int64                    `json:"pull_number"`
	HeadSHA               string                   `json:"head_sha"`
	ConfigurationRevision int64                    `json:"configuration_revision"`
	State                 string                   `json:"state"`
	Revision              int64                    `json:"revision"`
	Reason                string                   `json:"reason"`
	CreatedAt             string                   `json:"created_at"`
	UpdatedAt             string                   `json:"updated_at"`
	Observation           *ReviewSourceObservation `json:"observation,omitempty"`
	IncludeCI             bool                     `json:"include_ci,omitempty"`
	CI                    *ReviewSourceCISummary   `json:"ci,omitempty"`
}

// ReviewSourcePolicyAssessment evaluates policy as of a retained observation.
// It establishes neither current remote source nor CI or merge readiness.
type ReviewSourcePolicyAssessment struct {
	IncomingRoutesPending  int64                           `json:"incoming_routes_pending,omitempty"`
	IncomingRoutesCapacity bool                            `json:"incoming_routes_capacity,omitempty"`
	ProjectID              string                          `json:"project_id"`
	RefreshID              string                          `json:"refresh_id"`
	Source                 string                          `json:"source"`
	ConfigurationRevision  int64                           `json:"configuration_revision"`
	PolicyRevision         int64                           `json:"policy_revision"`
	SourceObservedAt       string                          `json:"source_observed_at"`
	EvaluatedAt            string                          `json:"evaluated_at"`
	State                  string                          `json:"state"`
	Target                 ReviewSourceObservation         `json:"target"`
	Channels               []ReviewPolicyAssessmentChannel `json:"channels"`
	Reason                 string                          `json:"reason,omitempty"`
}

func ValidateReviewSourceRequest(request ReviewSourceRequest) error {
	if request.ExpectedConfigurationRevision < 1 || !validPullNumber(request.PullNumber) || !controlHex(request.HeadSHA, 40) {
		return mutationValidation()
	}
	return nil
}

func ValidateReviewSourceRefreshID(id string) error {
	if !validReviewOpaqueID(id, "review_refresh_") {
		return validation()
	}
	return nil
}

func validSourceObservation(value ReviewSourceObservation) bool {
	parts := strings.Split(value.RepositoryName, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "." || part == ".." || len(part) < 1 || len(part) > 100 {
			return false
		}
		for _, r := range part {
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
				return false
			}
		}
	}
	return value.RepositoryID > 0 && validPullNumber(value.PullNumber) && controlHex(value.HeadSHA, 40) && controlHex(value.BaseSHA, 40) && controlHex(value.ParentSHA, 40) && value.ParentSHA == value.BaseSHA && controlText(value.BaseRef, 256, true) && value.ParentPullNumber >= 0 && value.ParentPullNumber <= 2147483647 && value.ParentPullNumber != value.PullNumber && controlTime(value.ObservedAt)
}

// RefreshReviewSource enqueues selected source and optional CI provider reads. It does not
// poll, retry, post a review or generate an idempotency key.
func (c *Client) RefreshReviewSource(ctx context.Context, key string, request ReviewSourceRequest, waitSeconds int) (ReviewSourceResult, error) {
	if !validMutationKey(key) || ValidateReviewSourceRequest(request) != nil || ValidateReviewSourceWait(waitSeconds) != nil {
		return ReviewSourceResult{}, mutationValidation()
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ReviewSourceResult{}, mutationValidation()
	}
	var value ReviewSourceResult
	var status int
	decode := func(data []byte, _ any) error {
		if status == http.StatusOK {
			if len(data) > 16<<10 {
				return errors.New("oversized source refresh operation")
			}
			value.Operation = &ReviewSourceOperation{}
			return decodeReviewResponse(data, value.Operation)
		}
		value.Receipt = &ReviewRequestReceipt{}
		return decodeReviewResponse(data, value.Receipt)
	}
	if err := c.requestBoundedOptions(ctx, http.MethodPost, c.projectPath("reviews", "source-refreshes"), nil, true, key, body, http.StatusAccepted, true, &value, 16<<10, decode, &sourceWaitOptions{seconds: waitSeconds, status: &status}); err != nil {
		return ReviewSourceResult{}, err
	}
	if value.Operation != nil {
		operation := value.Operation
		if !c.validSourceOperation(*operation) || operation.PullNumber != request.PullNumber || operation.HeadSHA != request.HeadSHA || operation.ConfigurationRevision != request.ExpectedConfigurationRevision || operation.IncludeCI != request.IncludeCI || !oneReviewOf(operation.State, "completed", "held") {
			return ReviewSourceResult{}, unknownMutation("invalid_response", "The service returned invalid source refresh facts; its outcome is unknown.", status)
		}
	} else if value.Receipt == nil || ValidateReviewSourceRefreshID(value.Receipt.ID) != nil || value.Receipt.State != "queued" || value.Receipt.Revision != 1 {
		return ReviewSourceResult{}, unknownMutation("invalid_response", "The service returned an invalid source refresh receipt; its outcome is unknown.", status)
	}
	return value, nil
}

func (c *Client) ReviewSourceOperation(ctx context.Context, id string) (ReviewSourceOperation, error) {
	if err := ValidateReviewSourceRefreshID(id); err != nil {
		return ReviewSourceOperation{}, err
	}
	var value ReviewSourceOperation
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "source-refreshes", id), nil, true, "", nil, http.StatusOK, false, &value, 16<<10, decodeReviewResponse); err != nil {
		return ReviewSourceOperation{}, err
	}
	if value.ID != id || !c.validSourceOperation(value) {
		return ReviewSourceOperation{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) validSourceOperation(value ReviewSourceOperation) bool {
	if ValidateReviewSourceRefreshID(value.ID) != nil || value.ProjectID != c.identity.ProjectID || !validPullNumber(value.PullNumber) || !controlHex(value.HeadSHA, 40) || value.ConfigurationRevision < 1 || !oneReviewOf(value.State, "queued", "running", "completed", "held") || value.Revision < 1 || value.Reason != "" && !validExecutionIdentifier(value.Reason) || !controlTime(value.CreatedAt) || !controlTime(value.UpdatedAt) {
		return false
	}
	// Retained timestamps may cross a backward host-clock step; revisions order state.
	return validSourceOperationCI(value) && (value.State == "completed") == (value.Observation != nil) && (value.Observation == nil || validSourceObservation(*value.Observation) && value.Observation.PullNumber == value.PullNumber && value.Observation.HeadSHA == value.HeadSHA)
}

func (c *Client) ReviewSourcePolicyAssessment(ctx context.Context, number int64) (ReviewSourcePolicyAssessment, error) {
	if !validPullNumber(number) {
		return ReviewSourcePolicyAssessment{}, validation()
	}
	var value ReviewSourcePolicyAssessment
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "source", strconv.FormatInt(number, 10), "policy"), nil, true, "", nil, http.StatusOK, false, &value, 16<<10, decodeReviewResponse); err != nil {
		return ReviewSourcePolicyAssessment{}, err
	}
	if value.ProjectID != c.identity.ProjectID || ValidateReviewSourceRefreshID(value.RefreshID) != nil || value.Source != "observed_comparison" || value.ConfigurationRevision < 0 || value.ConfigurationRevision == 0 && value.Reason != "source_configuration_changed" || !validSourceObservation(value.Target) || value.Target.PullNumber != number || value.SourceObservedAt != value.Target.ObservedAt || !controlTime(value.EvaluatedAt) || value.Channels == nil || len(value.Channels) > 2 || value.Reason != "" && !oneReviewOf(value.Reason, "source_configuration_changed", "routed_findings_pending", "routed_findings_capacity") {
		return ReviewSourcePolicyAssessment{}, invalidResponse()
	}
	if value.State == "unconfigured" {
		if value.PolicyRevision != 0 || len(value.Channels) != 0 || value.Reason != "" || value.IncomingRoutesPending != 0 || value.IncomingRoutesCapacity {
			return ReviewSourcePolicyAssessment{}, invalidResponse()
		}
		return value, nil
	}
	if value.Reason == "source_configuration_changed" {
		if value.State != "held" || len(value.Channels) != 0 || value.PolicyRevision < 0 || value.IncomingRoutesPending != 0 || value.IncomingRoutesCapacity {
			return ReviewSourcePolicyAssessment{}, invalidResponse()
		}
		return value, nil
	}
	if !validIncomingRoutePolicy(value.IncomingRoutesPending, value.IncomingRoutesCapacity, value.Reason) {
		return ReviewSourcePolicyAssessment{}, invalidResponse()
	}
	if value.PolicyRevision < 1 || !oneReviewOf(value.State, "sufficient", "held") || len(value.Channels) == 0 {
		return ReviewSourcePolicyAssessment{}, invalidResponse()
	}
	seen := map[string]bool{}
	sufficient := value.IncomingRoutesPending == 0 && !value.IncomingRoutesCapacity
	for _, channel := range value.Channels {
		if !oneReviewOf(channel.Channel, "cli", "hosted") || seen[channel.Channel] {
			return ReviewSourcePolicyAssessment{}, invalidResponse()
		}
		seen[channel.Channel] = true
		sufficient = sufficient && channel.Covered && !channel.Pending
	}
	if sufficient != (value.State == "sufficient") {
		return ReviewSourcePolicyAssessment{}, invalidResponse()
	}
	return value, nil
}
