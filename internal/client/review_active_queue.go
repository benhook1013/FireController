package client

import (
	"context"
	"net/http"

	"github.com/benhook1013/FireController/internal/reviewsize"
)

// ActiveQueueResponse contains only open, nonretired stored PR summaries.
type ActiveQueueResponse struct {
	ProjectID  string            `json:"project_id"`
	Configured bool              `json:"configured"`
	SnapshotID string            `json:"snapshot_id"`
	Revision   int64             `json:"revision"`
	Items      []ActiveQueueItem `json:"items"`
	Cursor     string            `json:"cursor,omitempty"`
	HasMore    bool              `json:"has_more"`
}
type ActiveQueueItem struct {
	ReviewSize   *reviewsize.Assessment `json:"review_size,omitempty"`
	Number       int64                  `json:"number"`
	Title        string                 `json:"title"`
	HeadSHA      string                 `json:"head_sha"`
	BaseRef      string                 `json:"base_ref"`
	CIState      string                 `json:"ci_state"`
	Mergeability string                 `json:"mergeability"`
	Position     int                    `json:"position"`
	RunCount     int64                  `json:"run_count"`
}

func (c *Client) ReviewActiveQueue(ctx context.Context, options PageOptions) (ActiveQueueResponse, error) {
	if options.Limit == 0 {
		options.Limit = 5
	}
	query, err := reviewPageQuery(options)
	if err != nil {
		return ActiveQueueResponse{}, err
	}
	var out ActiveQueueResponse
	if err = c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "queue", "active"), query, true, "", nil, 200, false, &out, maxReviewListResponse, decodeResponse); err != nil {
		return ActiveQueueResponse{}, err
	}
	if out.ProjectID != c.identity.ProjectID || !validReviewPage(out.Items, out.Cursor, out.HasMore, options) {
		return ActiveQueueResponse{}, invalidResponse()
	}
	if !out.Configured {
		if out.SnapshotID != "" || out.Revision != 0 || len(out.Items) != 0 || out.Cursor != "" || out.HasMore {
			return ActiveQueueResponse{}, invalidResponse()
		}
		return out, nil
	}
	if out.Revision < 1 || !validReviewSnapshot(out.SnapshotID) {
		return ActiveQueueResponse{}, invalidResponse()
	}
	seen := make(map[int64]bool)
	last := -1
	for _, item := range out.Items {
		if item.ReviewSize != nil && !reviewsize.Valid(*item.ReviewSize, item.Number) {
			return ActiveQueueResponse{}, invalidResponse()
		}
		if !validPullNumber(item.Number) || seen[item.Number] || !cleanReviewText(item.Title, 500, true) || !validReviewSHA(item.HeadSHA) || !cleanReviewText(item.BaseRef, 256, false) || !oneReviewOf(item.CIState, "unknown", "pending", "success", "failure", "not_required") || !oneReviewOf(item.Mergeability, "unknown", "mergeable", "conflicting") || item.Position < 0 || item.Position <= last || item.RunCount < 0 {
			return ActiveQueueResponse{}, invalidResponse()
		}
		seen[item.Number] = true
		last = item.Position
	}
	return out, nil
}
