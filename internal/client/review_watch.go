package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

const ReviewWatchDefaultSeconds = 300

type ReviewWatchOptions struct {
	PullNumber int64
	Channel    string
	Limit      int
	After      string
}
type ReviewWatchChange struct {
	Sequence  int64                  `json:"sequence"`
	EventType string                 `json:"event_type"`
	Revision  int64                  `json:"revision"`
	Operation ReviewOperationSummary `json:"operation"`
}
type ReviewWatchResult struct {
	ProjectID   string                `json:"project_id"`
	Snapshot    *ReviewOperationsPage `json:"snapshot,omitempty"`
	Changes     []ReviewWatchChange   `json:"changes"`
	EventCursor string                `json:"event_cursor"`
	HasMore     bool                  `json:"has_more"`
	TimedOut    bool                  `json:"timed_out"`
}

func ValidateReviewWatch(o ReviewWatchOptions, seconds int) error {
	if o.PullNumber != 0 && !validPullNumber(o.PullNumber) || o.Channel != "" && !oneReviewOf(o.Channel, "cli", "hosted") || o.Limit < 1 || o.Limit > 100 || o.After != "" && !validOpaqueCursor(o.After) || ValidateReviewOperationWait(seconds) != nil {
		return validation()
	}
	return nil
}
func (c *Client) WatchReviews(ctx context.Context, o ReviewWatchOptions, seconds int) (ReviewWatchResult, error) {
	if err := ValidateReviewWatch(o, seconds); err != nil {
		return ReviewWatchResult{}, err
	}
	q := url.Values{"limit": {strconv.Itoa(o.Limit)}}
	if o.PullNumber != 0 {
		q.Set("pull_number", strconv.FormatInt(o.PullNumber, 10))
	}
	if o.Channel != "" {
		q.Set("channel", o.Channel)
	}
	if o.After != "" {
		q.Set("after", o.After)
	} else {
		seconds = 0
	}
	var out ReviewWatchResult
	var status int
	err := resumeResultWait(ctx, seconds, func(ctx context.Context, remaining int) error {
		out = ReviewWatchResult{}
		return c.requestBoundedOptions(ctx, http.MethodGet, c.projectPath("reviews", "watch"), q, true, "", nil, http.StatusOK, false, &out, 256<<10, decodeReviewResponse, &sourceWaitOptions{seconds: remaining, status: &status, reviewWatch: true})
	})
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) {
			copy := *failure
			if failure.Code == "resync_required" {
				copy.Action = "Read a new reviews watch snapshot and handle its attention page; use its separate event_cursor for later waits. Inspect any additional attention pages explicitly."
				return ReviewWatchResult{}, &copy
			}
			if resultWaitShutdown(err) {
				copy.Action = "The service is restarting. Resume reviews watch with the same event cursor and PR/channel filters; no review or provider action was requested."
				return ReviewWatchResult{}, &copy
			}
		}
		return ReviewWatchResult{}, err
	}
	if !c.validReviewWatch(out, o) {
		return ReviewWatchResult{}, invalidResponse()
	}
	return out, nil
}
func (c *Client) validReviewWatch(v ReviewWatchResult, o ReviewWatchOptions) bool {
	if v.ProjectID != c.identity.ProjectID || v.Changes == nil || len(v.Changes) > o.Limit || !validOpaqueCursor(v.EventCursor) {
		return false
	}
	match := func(p ReviewOperationSummary) bool {
		return validReviewOperationSummary(p) && (o.PullNumber == 0 || p.PullNumber == o.PullNumber) && (o.Channel == "" || p.Channel == o.Channel)
	}
	if o.After == "" {
		if v.Snapshot == nil || v.Snapshot.Cursor != "" && !validOpaqueCursor(v.Snapshot.Cursor) || v.Snapshot.ProjectID != v.ProjectID || len(v.Changes) != 0 || v.HasMore || v.TimedOut || !validPage(Page[ReviewOperationSummary]{Items: v.Snapshot.Items, Cursor: v.Snapshot.Cursor, HasMore: v.Snapshot.HasMore}, PageOptions{Limit: o.Limit}) {
			return false
		}
		seen := map[string]bool{}
		for i, p := range v.Snapshot.Items {
			if !match(p) || seen[p.ID] || !oneReviewOf(p.State, "queued", "preparing", "posting", "active", "uncertain", "held", "completed") {
				return false
			}
			if i > 0 {
				prior := v.Snapshot.Items[i-1]
				if prior.CreatedAt < p.CreatedAt || prior.CreatedAt == p.CreatedAt && prior.ID <= p.ID {
					return false
				}
			}
			seen[p.ID] = true
		}
		return true
	}
	if v.Snapshot != nil || v.HasMore && len(v.Changes) != o.Limit || v.TimedOut && (len(v.Changes) > 0 || v.HasMore) || !v.TimedOut && len(v.Changes) == 0 {
		return false
	}
	var prior int64
	for _, change := range v.Changes {
		if !match(change.Operation) || change.Sequence <= prior || change.Revision < 1 || change.Revision > change.Operation.Revision || !oneReviewOf(change.EventType, "review.requested", "review.operation_updated") {
			return false
		}
		prior = change.Sequence
	}
	return true
}

// Agent output composes existing independently revisioned read contracts. The
// execution HTTP wire remains unchanged for running strict older clients.
type ReviewExecutionOutput struct {
	ReviewExecutionConfiguration
	Scheduling ReviewScheduling `json:"scheduling"`
}

func (c *Client) ReviewExecutionWithScheduling(ctx context.Context) (ReviewExecutionOutput, error) {
	configuration, err := c.ReviewExecution(ctx)
	if err != nil {
		return ReviewExecutionOutput{}, err
	}
	scheduling, err := c.ReviewScheduling(ctx)
	if err != nil {
		return ReviewExecutionOutput{}, err
	}
	return ReviewExecutionOutput{configuration, scheduling}, nil
}
