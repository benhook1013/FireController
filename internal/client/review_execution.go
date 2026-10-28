package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/benhook1013/FireController/internal/reviewsize"
)

// ReviewExecutionConfiguration contains stored project settings only. It does
// not assert that a provider is runnable or that review/CI requirements pass.
type ReviewExecutionConfiguration struct {
	ProjectID    string   `json:"project_id"`
	Revision     int64    `json:"revision"`
	CapabilityID string   `json:"capability_id"`
	Mode         string   `json:"mode"`
	Queue        []int64  `json:"queue"`
	Channels     []string `json:"channels"`
	Paused       bool     `json:"paused"`
}

type ReviewRequest struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	Channel                       string `json:"channel"`
	HeadSHA                       string `json:"head_sha"`
	AllowOutOfOrder               bool   `json:"allow_out_of_order,omitempty"`
}

type ReviewRequestReceipt struct {
	ReviewSize *reviewsize.Assessment `json:"review_size,omitempty"`
	ID         string                 `json:"id"`
	State      string                 `json:"state"`
	Revision   int64                  `json:"revision"`
}

// ReviewRequestResult contains exactly one flat wire result: the original
// durable receipt, or the authorized terminal operation collected by a wait.
type ReviewRequestResult struct {
	Receipt   *ReviewRequestReceipt
	Operation *ReviewOperation
}

func (r ReviewRequestResult) MarshalJSON() ([]byte, error) {
	if (r.Receipt == nil) == (r.Operation == nil) {
		return nil, errors.New("review request result requires exactly one result")
	}
	if r.Operation != nil {
		return json.Marshal(r.Operation)
	}
	return json.Marshal(r.Receipt)
}

type ReviewOperation struct {
	ID                    string                      `json:"id"`
	ProjectID             string                      `json:"project_id"`
	ActorID               string                      `json:"actor_id"`
	WorkerID              string                      `json:"worker_id"`
	ConfigurationRevision int64                       `json:"configuration_revision"`
	PolicyRevision        int64                       `json:"policy_revision"`
	CapabilityFingerprint string                      `json:"capability_fingerprint"`
	PullNumber            int64                       `json:"pull_number"`
	Channel               string                      `json:"channel"`
	HeadSHA               string                      `json:"head_sha"`
	AllowOutOfOrder       bool                        `json:"allow_out_of_order,omitempty"`
	State                 string                      `json:"state"`
	Revision              int64                       `json:"revision"`
	Reason                string                      `json:"reason"`
	AttemptID             string                      `json:"attempt_id"`
	CreatedAt             string                      `json:"created_at"`
	UpdatedAt             string                      `json:"updated_at"`
	Allowance             *ReviewAllowanceObservation `json:"allowance,omitempty"`
}

// ReviewAllowanceObservation describes this held attempt's retained facts, not
// current account availability or permission to start another review.
type ReviewAllowanceObservation struct {
	Kind             string `json:"kind"`
	ObservedAt       string `json:"observed_at"`
	Remaining        *int64 `json:"remaining,omitempty"`
	Limit            *int64 `json:"limit,omitempty"`
	FullCapacityInMS *int64 `json:"full_capacity_in_ms,omitempty"`
	RollingWindowMS  *int64 `json:"rolling_window_ms,omitempty"`
	RetryNotBefore   string `json:"retry_not_before,omitempty"`
}

func validAllowanceTime(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero() && parsed.UTC().Format(time.RFC3339Nano) == value
}

func validReviewAllowance(value ReviewOperation) bool {
	a := value.Allowance
	if a == nil {
		return true
	}
	if value.State != "held" || value.AttemptID == "" || !validAllowanceTime(a.ObservedAt) {
		return false
	}
	switch a.Kind {
	case "cli_exhausted":
		return value.Channel == "cli" && value.Reason == "allowance_exhausted" && a.Remaining != nil && *a.Remaining == 0 && a.Limit != nil && *a.Limit >= 1 && *a.Limit <= 10000 && a.FullCapacityInMS != nil && *a.FullCapacityInMS >= 0 && *a.FullCapacityInMS <= 86400000 && a.RollingWindowMS != nil && *a.RollingWindowMS >= 1 && *a.RollingWindowMS <= 86400000 && a.RetryNotBefore == ""
	case "hosted_cooldown":
		return value.Channel == "hosted" && value.Reason == "hosted_allowance_cooldown" && a.Remaining == nil && a.Limit == nil && a.FullCapacityInMS == nil && a.RollingWindowMS == nil && validAllowanceTime(a.RetryNotBefore)
	}
	return false
}

func validExecutionIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// ValidateReviewRequest admits explicit request assertions before transport I/O.
func ValidateReviewRequest(request ReviewRequest) error {
	if request.ExpectedConfigurationRevision < 1 || !validPullNumber(request.PullNumber) || !oneReviewOf(request.Channel, "cli", "hosted") || !controlHex(request.HeadSHA, 40) {
		return mutationValidation()
	}
	return nil
}

func ValidateReviewOperationID(id string) error {
	if !validReviewOpaqueID(id, "review_operation_") {
		return validation()
	}
	return nil
}

// ReviewExecution reads stored configuration without refreshing any provider.
func (c *Client) ReviewExecution(ctx context.Context) (ReviewExecutionConfiguration, error) {
	var value ReviewExecutionConfiguration
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "execution", "configuration"), nil, true, "", nil, http.StatusOK, false, &value, 4<<20, decodeReviewResponse); err != nil {
		return ReviewExecutionConfiguration{}, err
	}
	if value.ProjectID != c.identity.ProjectID || !validExecutionIdentifier(value.ProjectID) || value.Revision < 1 || !validExecutionIdentifier(value.CapabilityID) || !oneReviewOf(value.Mode, "independent", "stacked") || value.Queue == nil || len(value.Queue) < 1 || len(value.Queue) > 100 || value.Channels == nil || len(value.Channels) < 1 || len(value.Channels) > 2 {
		return ReviewExecutionConfiguration{}, invalidResponse()
	}
	numbers := make(map[int64]bool, len(value.Queue))
	for _, number := range value.Queue {
		if !validPullNumber(number) || numbers[number] {
			return ReviewExecutionConfiguration{}, invalidResponse()
		}
		numbers[number] = true
	}
	channels := make(map[string]bool, len(value.Channels))
	for _, channel := range value.Channels {
		if !oneReviewOf(channel, "cli", "hosted") || channels[channel] {
			return ReviewExecutionConfiguration{}, invalidResponse()
		}
		channels[channel] = true
	}
	return value, nil
}

// RequestReview authorizes background provider work. It submits once and never
// generates a key or retries an unknown outcome; callers retain the exact key.
func (c *Client) RequestReview(ctx context.Context, key string, request ReviewRequest, waitSeconds int) (ReviewRequestResult, error) {
	if !validMutationKey(key) || ValidateReviewRequest(request) != nil || ValidateReviewOperationWait(waitSeconds) != nil {
		return ReviewRequestResult{}, mutationValidation()
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ReviewRequestResult{}, mutationValidation()
	}
	var result ReviewRequestResult
	var status int
	decode := func(data []byte, _ any) error {
		if status == http.StatusOK {
			result.Operation = &ReviewOperation{}
			return decodeReviewResponse(data, result.Operation)
		}
		result.Receipt = &ReviewRequestReceipt{}
		return decodeReviewResponse(data, result.Receipt)
	}
	if err := c.requestBoundedOptions(ctx, http.MethodPost, c.projectPath("reviews", "requests"), nil, true, key, body, http.StatusAccepted, true, &result, 4<<20, decode, &sourceWaitOptions{seconds: waitSeconds, status: &status}); err != nil {
		return ReviewRequestResult{}, err
	}
	valid := false
	if operation := result.Operation; operation != nil {
		valid = c.validReviewOperation(*operation) && operation.WorkerID == c.identity.WorkerID && operation.PullNumber == request.PullNumber && operation.HeadSHA == request.HeadSHA && operation.Channel == request.Channel && operation.AllowOutOfOrder == request.AllowOutOfOrder && operation.ConfigurationRevision == request.ExpectedConfigurationRevision && !oneReviewOf(operation.State, "queued", "preparing", "posting", "active")
	} else if receipt := result.Receipt; receipt != nil {
		valid = ValidateReviewOperationID(receipt.ID) == nil && receipt.State == "queued" && receipt.Revision == 1 && (receipt.ReviewSize == nil || reviewsize.Valid(*receipt.ReviewSize, request.PullNumber))
	}
	if !valid {
		return ReviewRequestResult{}, unknownMutation("invalid_response", "The service returned invalid review request facts; its outcome is unknown.", status)
	}
	return result, nil
}

// ReviewOperation reads retained state in the bound project. A different worker
// in that same project is valid attribution, rather than a binding mismatch.
func (c *Client) ReviewOperation(ctx context.Context, id string) (ReviewOperation, error) {
	return c.WaitReviewOperation(ctx, id, 0)
}

// WaitReviewOperation reads retained state, optionally waiting for a terminal
// transition. Detaching a read never cancels or retries the durable operation.
func (c *Client) WaitReviewOperation(ctx context.Context, id string, waitSeconds int) (ReviewOperation, error) {
	if ValidateReviewOperationID(id) != nil || ValidateReviewOperationWait(waitSeconds) != nil {
		return ReviewOperation{}, validation()
	}
	var value ReviewOperation
	var status int
	if err := c.requestBoundedOptions(ctx, http.MethodGet, c.projectPath("reviews", "operations", id), nil, true, "", nil, http.StatusOK, false, &value, 4<<20, decodeReviewResponse, &sourceWaitOptions{seconds: waitSeconds, status: &status}); err != nil {
		return ReviewOperation{}, err
	}
	if value.ID != id || !c.validReviewOperation(value) {
		return ReviewOperation{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) validReviewOperation(value ReviewOperation) bool {
	return ValidateReviewOperationID(value.ID) == nil && value.ProjectID == c.identity.ProjectID && validExecutionIdentifier(value.ProjectID) && validExecutionIdentifier(value.ActorID) && (value.WorkerID == "" || validExecutionIdentifier(value.WorkerID)) && value.ConfigurationRevision >= 1 && value.PolicyRevision >= 0 && (value.CapabilityFingerprint == "" || controlHex(value.CapabilityFingerprint, 64)) && validPullNumber(value.PullNumber) && oneReviewOf(value.Channel, "cli", "hosted") && controlHex(value.HeadSHA, 40) && oneReviewOf(value.State, "queued", "preparing", "posting", "active", "completed", "failed", "cancelled", "rate_limited", "uncertain", "held") && value.Revision >= 1 && (value.Reason == "" || validExecutionIdentifier(value.Reason)) && (value.AttemptID == "" || validReviewOpaqueID(value.AttemptID, "review_attempt_")) && controlTime(value.CreatedAt) && controlTime(value.UpdatedAt) && validReviewAllowance(value)
}
