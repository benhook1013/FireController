package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
)

type ReviewQueueChange struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	Before                        *int64 `json:"before,omitempty"`
	After                         *int64 `json:"after,omitempty"`
	Position                      string `json:"position,omitempty"`
}

func ValidateReviewQueueChange(action string, r ReviewQueueChange) error {
	if !oneReviewOf(action, "add", "move") || r.ExpectedConfigurationRevision < 1 || r.ExpectedConfigurationRevision == math.MaxInt64 || !validPullNumber(r.PullNumber) {
		return mutationValidation()
	}
	n := 0
	for _, anchor := range []*int64{r.Before, r.After} {
		if anchor != nil {
			n++
			if !validPullNumber(*anchor) || *anchor == r.PullNumber {
				return mutationValidation()
			}
		}
	}
	if r.Position != "" {
		n++
		if !oneReviewOf(r.Position, "first", "last") {
			return mutationValidation()
		}
	}
	if n > 1 || action == "move" && n != 1 {
		return mutationValidation()
	}
	return nil
}
func (c *Client) ChangeReviewQueue(ctx context.Context, action, key string, r ReviewQueueChange) (ReviewRequestReceipt, error) {
	var out ReviewRequestReceipt
	if !validMutationKey(key) {
		return out, mutationValidation()
	}
	if err := ValidateReviewQueueChange(action, r); err != nil {
		return out, err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return out, mutationValidation()
	}
	if err = c.requestBounded(ctx, http.MethodPost, c.projectPath("reviews", "execution", "queue", action), nil, true, key, body, http.StatusCreated, true, &out, 16<<10, decodeReviewResponse); err != nil {
		return ReviewRequestReceipt{}, queueChangeError(err)
	}
	want := r.ExpectedConfigurationRevision + 1
	if out.State == "unchanged" {
		want = r.ExpectedConfigurationRevision
	}
	if out.ID != c.identity.ProjectID || out.Revision != want || !oneReviewOf(out.State, "configured", "unchanged") || action == "add" && out.State == "unchanged" {
		return ReviewRequestReceipt{}, unknownMutation("invalid_response", "The queue-change receipt was invalid; recover the original action with its same arguments, key and binding.", http.StatusCreated)
	}
	return out, nil
}

// Known queue refusals describe the smallest useful local action; they are not
// evidence that a verified profile or credential is wrong.
func queueChangeError(err error) error {
	var e *Error
	if !errors.As(err, &e) || e.OutcomeUnknown {
		return err
	}
	switch e.Code {
	case "review_queue_duplicate":
		e.Message = "This PR is already in the configured queue."
		e.Action = "Use reviews queue-move (MCP review_queue_move) with the current configuration revision if its position should change. No queue change was made."
	case "review_queue_anchor_missing":
		e.Message = "The selected destination PR is not in the configured queue."
		e.Action = "Read reviews execution (MCP review_execution_read), then select an existing before/after anchor or first/last with its current revision. No queue change was made."
	case "review_queue_full":
		e.Message = "The configured scheduling queue already contains 100 PRs."
		e.Action = "Deliberately update scheduling with the project operator before adding another PR; retain completed review history. This refusal did not change the queue or require new credentials."
	case "review_target_retired":
		e.Message = "This PR was explicitly retired."
		e.Action = "Confirm deliberate reactivation with the project operator before enrolling it again. Retained review history is preserved; no queue change was made."
	case "review_not_configured":
		e.Message = "The selected PR is not enrolled in the configured queue."
		e.Action = "Read reviews execution (MCP review_execution_read). Use reviews queue-add (MCP review_queue_add) for the intended missing PR with that current revision and an explicit key. No queue change was made."
	case "reviews_unconfigured":
		e.Message = "This project has no execution configuration yet."
		e.Action = "Complete initial project execution setup with the operator, then use the ordinary queue tools. No queue change was made; switching worker identity is unnecessary."
	}
	return e
}
