package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
)

// ReviewRunRequest authorizes one channel-front selection, never a queue drain.
type ReviewRunRequest struct {
	Channel                       string `json:"channel"`
	ExpectedPullNumber            *int64 `json:"expected_pull_number,omitempty"`
	ExpectedConfigurationRevision *int64 `json:"expected_configuration_revision,omitempty"`
}

type ReviewRunSelection struct {
	ID                    string `json:"id"`
	ProjectID             string `json:"project_id"`
	ActorID               string `json:"actor_id"`
	WorkerID              string `json:"worker_id"`
	Channel               string `json:"channel"`
	ConfigurationRevision int64  `json:"configuration_revision"`
	PolicyRevision        int64  `json:"policy_revision"`
	ExpectedPullNumber    int64  `json:"expected_pull_number"`
	State                 string `json:"state"`
	Reason                string `json:"reason"`
	Revision              int64  `json:"revision"`
	OperationID           string `json:"operation_id,omitempty"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

type ReviewRunState struct {
	Selection ReviewRunSelection `json:"selection"`
	Operation *ReviewOperation   `json:"operation,omitempty"`
}

// ReviewRunResult marshals the exact flat receipt or selected result envelope.
type ReviewRunResult struct {
	Receipt *ReviewRequestReceipt
	Result  *ReviewRunState
}

func (r ReviewRunResult) MarshalJSON() ([]byte, error) {
	if (r.Receipt == nil) == (r.Result == nil) {
		return nil, errors.New("review run requires exactly one result")
	}
	if r.Result != nil {
		return json.Marshal(r.Result)
	}
	return json.Marshal(r.Receipt)
}

func ValidateReviewRunRequest(r ReviewRunRequest) error {
	if !oneReviewOf(r.Channel, "cli", "hosted") || r.ExpectedPullNumber != nil && !validPullNumber(*r.ExpectedPullNumber) || r.ExpectedConfigurationRevision != nil && (*r.ExpectedConfigurationRevision < 1 || *r.ExpectedConfigurationRevision == math.MaxInt64) {
		return mutationValidation()
	}
	return nil
}
func ValidateReviewRunSelectionID(id string) error {
	if !validReviewOpaqueID(id, "review_selection_") {
		return validation()
	}
	return nil
}

func (c *Client) validReviewRun(s ReviewRunState) bool {
	r := s.Selection
	if ValidateReviewRunSelectionID(r.ID) != nil || r.ProjectID != c.identity.ProjectID || !validExecutionIdentifier(r.ProjectID) || !validExecutionIdentifier(r.ActorID) || !validExecutionIdentifier(r.WorkerID) || !oneReviewOf(r.Channel, "cli", "hosted") || r.ConfigurationRevision < 1 || r.PolicyRevision < 1 || r.ExpectedPullNumber != 0 && !validPullNumber(r.ExpectedPullNumber) || r.Revision < 1 || !controlTime(r.CreatedAt) || !controlTime(r.UpdatedAt) {
		return false
	}
	switch r.State {
	case "queued", "selecting":
		if r.Reason != "" {
			return false
		}
	case "held":
		if !validExecutionIdentifier(r.Reason) {
			return false
		}
	case "empty":
		if r.Reason != "queue_complete" {
			return false
		}
	case "selected":
		op := s.Operation
		return r.Reason == "" && ValidateReviewOperationID(r.OperationID) == nil && op != nil && c.validReviewOperation(*op) && !op.AllowOutOfOrder && op.ID == r.OperationID && op.ProjectID == r.ProjectID && op.ActorID == r.ActorID && op.WorkerID == r.WorkerID && op.Channel == r.Channel && op.ConfigurationRevision == r.ConfigurationRevision && op.PolicyRevision == r.PolicyRevision && (r.ExpectedPullNumber == 0 || op.PullNumber == r.ExpectedPullNumber)
	default:
		return false
	}
	return r.OperationID == "" && s.Operation == nil
}
func reviewRunPending(s ReviewRunState) bool {
	return oneReviewOf(s.Selection.State, "queued", "selecting") || s.Operation != nil && oneReviewOf(s.Operation.State, "queued", "preparing", "posting", "active")
}

// RunReview submits once. Waiting only collects its retained result; detaching
// never authorizes a retry, another selection or cancellation of provider work.
func (c *Client) RunReview(ctx context.Context, key string, r ReviewRunRequest, waitSeconds int) (ReviewRunResult, error) {
	if !validMutationKey(key) || ValidateReviewRunRequest(r) != nil || ValidateReviewOperationWait(waitSeconds) != nil {
		return ReviewRunResult{}, mutationValidation()
	}
	body, err := json.Marshal(r)
	if err != nil {
		return ReviewRunResult{}, mutationValidation()
	}
	var result ReviewRunResult
	var status int
	decode := func(raw []byte, _ any) error {
		if status == http.StatusOK {
			result.Result = &ReviewRunState{}
			return decodeReviewResponse(raw, result.Result)
		}
		result.Receipt = &ReviewRequestReceipt{}
		return decodeReviewResponse(raw, result.Receipt)
	}
	if err = c.requestBoundedOptions(ctx, http.MethodPost, c.projectPath("reviews", "selections"), nil, true, key, body, http.StatusAccepted, true, &result, 64<<10, decode, &sourceWaitOptions{seconds: waitSeconds, status: &status}); err != nil {
		return ReviewRunResult{}, err
	}
	valid := false
	if state := result.Result; state != nil {
		selection := state.Selection
		valid = c.validReviewRun(*state) && !reviewRunPending(*state) && selection.WorkerID == c.identity.WorkerID && selection.Channel == r.Channel
		if r.ExpectedPullNumber != nil {
			valid = valid && selection.ExpectedPullNumber == *r.ExpectedPullNumber
		} else {
			valid = valid && selection.ExpectedPullNumber == 0
		}
		if r.ExpectedConfigurationRevision != nil {
			valid = valid && selection.ConfigurationRevision == *r.ExpectedConfigurationRevision
		}
	} else if receipt := result.Receipt; receipt != nil {
		valid = ValidateReviewRunSelectionID(receipt.ID) == nil && receipt.State == "queued" && receipt.Revision == 1
	}
	if !valid {
		return ReviewRunResult{}, unknownMutation("invalid_response", "The service returned invalid selection facts; its outcome is unknown.", status)
	}
	return result, nil
}

func (c *Client) ReviewSelection(ctx context.Context, id string) (ReviewRunState, error) {
	return c.WaitReviewSelection(ctx, id, 0)
}
func (c *Client) WaitReviewSelection(ctx context.Context, id string, waitSeconds int) (ReviewRunState, error) {
	if ValidateReviewRunSelectionID(id) != nil || ValidateReviewOperationWait(waitSeconds) != nil {
		return ReviewRunState{}, validation()
	}
	var result ReviewRunState
	var status int
	if err := c.requestBoundedOptions(ctx, http.MethodGet, c.projectPath("reviews", "selections", id), nil, true, "", nil, http.StatusOK, false, &result, 64<<10, decodeReviewResponse, &sourceWaitOptions{seconds: waitSeconds, status: &status}); err != nil {
		return ReviewRunState{}, err
	}
	if result.Selection.ID != id || !c.validReviewRun(result) {
		return ReviewRunState{}, invalidResponse()
	}
	return result, nil
}
