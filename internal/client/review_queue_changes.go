package client

import (
	"context"
	"encoding/json"
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
		return ReviewRequestReceipt{}, err
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
