package client

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/strictjson"
	"math"
	"net/http"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

type ReviewScheduling struct {
	ProjectID           string                 `json:"project_id"`
	Revision            int64                  `json:"revision"`
	AutomaticCLI        bool                   `json:"automatic_cli"`
	AutomaticHosted     bool                   `json:"automatic_hosted"`
	PausedPulls         []ReviewPullScheduling `json:"paused_pulls"`
	AuthorizingWorkerID string                 `json:"authorizing_worker_id,omitempty"`
	UpdatedAt           string                 `json:"updated_at,omitempty"`
	Reason              string                 `json:"reason,omitempty"`
}

type ReviewPullScheduling struct {
	ProjectID  string `json:"project_id"`
	PullNumber int64  `json:"pull_number"`
	Revision   int64  `json:"revision"`
	Paused     bool   `json:"paused"`
	Reason     string `json:"reason"`
	ActorID    string `json:"actor_id,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

type ConfigureReviewSchedulingRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
	AutomaticHosted  bool  `json:"automatic_hosted"`
	AutomaticCLI     *bool `json:"automatic_cli,omitempty"`
}

type ConfigureReviewPullSchedulingRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Paused           bool   `json:"paused"`
	Reason           string `json:"reason,omitempty"`
}

func validSchedulingRevision(revision int64) bool { return revision >= 0 && revision < math.MaxInt64 }
func validSchedulingTime(value string) bool {
	if len(value) > 64 {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !at.IsZero()
}
func validSchedulingReason(reason string) bool {
	if len(reason) > 256 || !utf8.ValidString(reason) {
		return false
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func ValidateConfigureReviewScheduling(input ConfigureReviewSchedulingRequest) error {
	if !validSchedulingRevision(input.ExpectedRevision) {
		return mutationValidation()
	}
	return nil
}

func ValidateConfigureReviewPullScheduling(pull int64, input ConfigureReviewPullSchedulingRequest) error {
	if !validPullNumber(pull) || !validSchedulingRevision(input.ExpectedRevision) || !validSchedulingReason(input.Reason) {
		return mutationValidation()
	}
	return nil
}

func validPullScheduling(value ReviewPullScheduling, project string, pull int64) bool {
	if value.ProjectID != project || value.PullNumber != pull || !validPullNumber(pull) || value.Revision < 0 || !validSchedulingReason(value.Reason) || !value.Paused && value.Reason != "" {
		return false
	}
	if value.Revision == 0 {
		return !value.Paused && value.Reason == "" && value.ActorID == "" && value.UpdatedAt == ""
	}
	return validID(value.ActorID) && validSchedulingTime(value.UpdatedAt)
}

// Older services omit automatic_cli. Accept that as off while always emitting
// an explicit boolean in CLI/MCP output, including after an enabled read.
func decodeReviewSchedulingResponse(data []byte, dst any) error {
	if err := strictjson.Decode(data, dst); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return strictjson.ErrSyntax
	}
	if _, present := fields["automatic_cli"]; !present {
		fields["automatic_cli"] = json.RawMessage("false")
		var err error
		data, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	}
	if !completeShape(data, dst) {
		return strictjson.ErrSyntax
	}
	return nil
}

func (c *Client) ReviewScheduling(ctx context.Context) (ReviewScheduling, error) {
	// Allow 100 paused pulls, including six-byte JSON escaping of each
	// 256-byte reason and bounded identities, plus the response envelope.
	var value ReviewScheduling
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "scheduling"), nil, true, "", nil, http.StatusOK, false, &value, 512<<10, decodeReviewSchedulingResponse); err != nil {
		return ReviewScheduling{}, err
	}
	if value.ProjectID != c.identity.ProjectID || value.Revision < 0 || value.PausedPulls == nil || len(value.PausedPulls) > 100 || value.Reason != "" && !safeCode(value.Reason) || value.AuthorizingWorkerID != "" && !validID(value.AuthorizingWorkerID) {
		return ReviewScheduling{}, invalidResponse()
	}
	if value.Revision == 0 {
		if value.AutomaticHosted || value.AutomaticCLI || value.AuthorizingWorkerID != "" || value.UpdatedAt != "" || value.Reason != "" {
			return ReviewScheduling{}, invalidResponse()
		}
	} else if !validSchedulingTime(value.UpdatedAt) || (value.AutomaticHosted || value.AutomaticCLI) && value.AuthorizingWorkerID == "" {
		return ReviewScheduling{}, invalidResponse()
	}
	seen := make(map[int64]bool, len(value.PausedPulls))
	for _, item := range value.PausedPulls {
		if !item.Paused || seen[item.PullNumber] || !validPullScheduling(item, c.identity.ProjectID, item.PullNumber) {
			return ReviewScheduling{}, invalidResponse()
		}
		seen[item.PullNumber] = true
	}
	return value, nil
}

func (c *Client) ConfigureReviewScheduling(ctx context.Context, key string, input ConfigureReviewSchedulingRequest) (ReviewRequestReceipt, error) {
	if !validMutationKey(key) || ValidateConfigureReviewScheduling(input) != nil {
		return ReviewRequestReceipt{}, mutationValidation()
	}
	var receipt ReviewRequestReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "scheduling"), key, input, http.StatusOK, &receipt); err != nil {
		return receipt, err
	}
	if receipt.ID != c.identity.ProjectID || receipt.State != "scheduling_configured" || receipt.Revision != input.ExpectedRevision+1 {
		return ReviewRequestReceipt{}, unknownMutation("invalid_response", "The scheduling receipt was invalid; retain the original key and request.", http.StatusOK)
	}
	return receipt, nil
}

func (c *Client) ReviewPullScheduling(ctx context.Context, pull int64) (ReviewPullScheduling, error) {
	if !validPullNumber(pull) {
		return ReviewPullScheduling{}, validation()
	}
	var value ReviewPullScheduling
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "pulls", strconv.FormatInt(pull, 10), "scheduling"), nil, true, "", nil, http.StatusOK, false, &value, 4<<10, decodeResponse); err != nil {
		return ReviewPullScheduling{}, err
	}
	if !validPullScheduling(value, c.identity.ProjectID, pull) {
		return ReviewPullScheduling{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) ConfigureReviewPullScheduling(ctx context.Context, key string, pull int64, input ConfigureReviewPullSchedulingRequest) (ReviewRequestReceipt, error) {
	if !validMutationKey(key) || ValidateConfigureReviewPullScheduling(pull, input) != nil {
		return ReviewRequestReceipt{}, mutationValidation()
	}
	var receipt ReviewRequestReceipt
	id := strconv.FormatInt(pull, 10)
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "pulls", id, "scheduling"), key, input, http.StatusOK, &receipt); err != nil {
		return receipt, err
	}
	if receipt.ID != id || receipt.State != "pull_scheduling_updated" || receipt.Revision != input.ExpectedRevision+1 {
		return ReviewRequestReceipt{}, unknownMutation("invalid_response", "The PR scheduling receipt was invalid; retain the original key and request.", http.StatusOK)
	}
	return receipt, nil
}
