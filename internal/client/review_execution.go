package client

import (
	"context"
	"encoding/json"
	"net/http"
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
}

type ReviewRequestReceipt struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}

type ReviewOperation struct {
	ID                    string `json:"id"`
	ProjectID             string `json:"project_id"`
	ActorID               string `json:"actor_id"`
	WorkerID              string `json:"worker_id"`
	ConfigurationRevision int64  `json:"configuration_revision"`
	PolicyRevision        int64  `json:"policy_revision"`
	CapabilityFingerprint string `json:"capability_fingerprint"`
	PullNumber            int64  `json:"pull_number"`
	Channel               string `json:"channel"`
	HeadSHA               string `json:"head_sha"`
	State                 string `json:"state"`
	Revision              int64  `json:"revision"`
	Reason                string `json:"reason"`
	AttemptID             string `json:"attempt_id"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
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
func (c *Client) RequestReview(ctx context.Context, key string, request ReviewRequest) (ReviewRequestReceipt, error) {
	if !validMutationKey(key) || ValidateReviewRequest(request) != nil {
		return ReviewRequestReceipt{}, mutationValidation()
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ReviewRequestReceipt{}, mutationValidation()
	}
	var result ReviewRequestReceipt
	if err := c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "requests"), nil, true, key, body, http.StatusAccepted, true, &result, 4<<20, decodeReviewResponse); err != nil {
		return ReviewRequestReceipt{}, err
	}
	if ValidateReviewOperationID(result.ID) != nil || result.State != "queued" || result.Revision != 1 {
		return ReviewRequestReceipt{}, unknownMutation("invalid_response", "The service returned an invalid review request receipt; its outcome is unknown.", http.StatusAccepted)
	}
	return result, nil
}

// ReviewOperation reads retained state in the bound project. A different worker
// in that same project is valid attribution, rather than a binding mismatch.
func (c *Client) ReviewOperation(ctx context.Context, id string) (ReviewOperation, error) {
	if err := ValidateReviewOperationID(id); err != nil {
		return ReviewOperation{}, err
	}
	var value ReviewOperation
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "operations", id), nil, true, "", nil, http.StatusOK, false, &value, 4<<20, decodeReviewResponse); err != nil {
		return ReviewOperation{}, err
	}
	if value.ID != id || value.ProjectID != c.identity.ProjectID || !validExecutionIdentifier(value.ProjectID) || !validExecutionIdentifier(value.ActorID) || value.WorkerID != "" && !validExecutionIdentifier(value.WorkerID) || value.ConfigurationRevision < 1 || value.PolicyRevision < 0 || value.CapabilityFingerprint != "" && !controlHex(value.CapabilityFingerprint, 64) || !validPullNumber(value.PullNumber) || !oneReviewOf(value.Channel, "cli", "hosted") || !controlHex(value.HeadSHA, 40) || !oneReviewOf(value.State, "queued", "preparing", "posting", "active", "completed", "failed", "cancelled", "rate_limited", "uncertain", "held") || value.Revision < 1 || value.Reason != "" && !validExecutionIdentifier(value.Reason) || value.AttemptID != "" && !validReviewOpaqueID(value.AttemptID, "review_attempt_") || !controlTime(value.CreatedAt) || !controlTime(value.UpdatedAt) {
		return ReviewOperation{}, invalidResponse()
	}
	return value, nil
}
