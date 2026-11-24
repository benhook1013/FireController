package client

import (
	"context"
	"encoding/json"
	"net/http"
)

type ReviewQueuePriority struct {
	ProjectID     string  `json:"project_id"`
	Revision      int64   `json:"revision"`
	PriorityPulls []int64 `json:"priority_pulls"`
}

type ReviewQueuePriorityChange struct {
	ExpectedRevision int64   `json:"expected_revision"`
	PullNumbers      []int64 `json:"pull_numbers"`
	Priority         bool    `json:"priority"`
}

func (c *Client) ReadReviewQueuePriority(ctx context.Context) (ReviewQueuePriority, error) {
	var out ReviewQueuePriority
	err := c.get(ctx, c.projectPath("reviews", "queue-priority"), nil, true, &out)
	return out, err
}

func (c *Client) SetReviewQueuePriority(ctx context.Context, key string, change ReviewQueuePriorityChange) (ReviewQueuePriority, error) {
	var out ReviewQueuePriority
	body, err := json.Marshal(change)
	if err != nil {
		return out, err
	}
	err = c.request(ctx, http.MethodPost, c.projectPath("reviews", "queue-priority"), nil, true, key, body, http.StatusOK, true, &out)
	return out, err
}
