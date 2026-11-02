package client

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

type ReviewFollowing struct {
	ProjectID  string `json:"project_id"`
	PullNumber int64  `json:"pull_number"`
	WorkerID   string `json:"worker_id"`
	Mode       string `json:"mode"`
	Revision   int64  `json:"revision"`
	Tracked    bool   `json:"tracked"`
}
type ConfigureReviewFollowingRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
	Following        bool  `json:"following"`
}

func (c *Client) validReviewFollowing(v ReviewFollowing, number int64) bool {
	return v.ProjectID == c.identity.ProjectID && v.WorkerID == c.identity.WorkerID && v.PullNumber == number && (v.Mode == "none" && v.Revision == 0 || oneReviewOf(v.Mode, "auto", "follow", "muted") && v.Revision > 0)
}
func (c *Client) ReviewFollowing(ctx context.Context, number int64) (ReviewFollowing, error) {
	var out ReviewFollowing
	if !validPullNumber(number) {
		return out, validation()
	}
	if err := c.get(ctx, c.projectPath("reviews", "pulls", strconv.FormatInt(number, 10), "following"), nil, true, &out); err != nil {
		return out, err
	}
	if !c.validReviewFollowing(out, number) {
		return ReviewFollowing{}, invalidResponse()
	}
	return out, nil
}
func (c *Client) ConfigureReviewFollowing(ctx context.Context, key string, number int64, input ConfigureReviewFollowingRequest) (ReviewFollowing, error) {
	var out ReviewFollowing
	if !validMutationKey(key) || !validPullNumber(number) || input.ExpectedRevision < 0 || input.ExpectedRevision == math.MaxInt64 {
		return out, mutationValidation()
	}
	body, _ := json.Marshal(input)
	if err := c.requestBounded(ctx, http.MethodPut, c.projectPath("reviews", "pulls", strconv.FormatInt(number, 10), "following"), nil, true, key, body, 200, true, &out, 16<<10, decodeReviewResponse); err != nil {
		return out, err
	}
	want := "muted"
	if input.Following {
		want = "follow"
	}
	if !c.validReviewFollowing(out, number) || out.Mode != want || out.Revision < input.ExpectedRevision || out.Revision > input.ExpectedRevision+1 {
		return ReviewFollowing{}, unknownMutation("invalid_response", "The following preference could not be verified; retry the same request, key and binding.", 200)
	}
	return out, nil
}
