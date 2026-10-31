package client

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/benhook1013/FireController/internal/reviewsize"
)

type ReviewQueueChange struct {
	ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
	PullNumber                    int64  `json:"pull_number"`
	Before                        *int64 `json:"before,omitempty"`
	After                         *int64 `json:"after,omitempty"`
	Position                      string `json:"position,omitempty"`
	Reason                        string `json:"reason,omitempty"`
}

func ValidateReviewQueueChange(action string, r ReviewQueueChange) error {
	if !oneReviewOf(action, "add", "move", "remove") || r.ExpectedConfigurationRevision < 1 || r.ExpectedConfigurationRevision == math.MaxInt64 || !validPullNumber(r.PullNumber) {
		return mutationValidation()
	}
	if action == "remove" {
		if !oneReviewOf(r.Reason, "superseded", "defunct") || r.Before != nil || r.After != nil || r.Position != "" {
			return mutationValidation()
		}
		return nil
	}
	if r.Reason != "" {
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
	if out.ReviewSize != nil && !reviewsize.Valid(*out.ReviewSize, r.PullNumber) {
		return ReviewRequestReceipt{}, unknownMutation("invalid_response", "The queue sizing information could not be verified; preserve the original action key.", http.StatusCreated)
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
		e.Action = "Deliberately remove a defunct or superseded PR with reviews queue-remove (MCP review_queue_remove) before adding another; retain completed review history. No queue change was made."
	case "review_queue_operation_pending":
		e.Message = "This PR has a pending review operation; queue removal does not cancel it."
		e.Action = "Read the retained operation identified by operation_id and let it finish or use its supported deliberate recovery. Then retry removal with the current configuration revision and a new key. No queue change was made."
	case "review_queue_merged":
		e.Message = "Merged PRs stay in completed queue history."
		e.Action = "Do not remove a merged PR as routine supersession cleanup. Its history is retained; no queue change was made."
	case "review_queue_target_missing":
		e.Message = "The selected PR has no visible queue entry to retire."
		e.Action = "Read the current configured queue and retained history before selecting a removal target. No queue change was made."
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
