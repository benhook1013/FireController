package client

import (
	"context"
	"strconv"
)

type ReviewOperationsOptions struct {
	PageOptions
	PullNumber int64
	Channel    string
	Status     string
}

type ReviewOperationSummary struct {
	ID         string `json:"id"`
	PullNumber int64  `json:"pull_number"`
	Channel    string `json:"channel"`
	HeadSHA    string `json:"head_sha"`
	WorkerID   string `json:"worker_id"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	AttemptID  string `json:"attempt_id"`
	Revision   int64  `json:"revision"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type ReviewOperationsPage struct {
	ProjectID string                   `json:"project_id"`
	Items     []ReviewOperationSummary `json:"items"`
	Cursor    string                   `json:"cursor,omitempty"`
	HasMore   bool                     `json:"has_more"`
}

func ValidateReviewOperationsOptions(o ReviewOperationsOptions) error {
	if o.PullNumber != 0 && !validPullNumber(o.PullNumber) || o.Channel != "" && !oneReviewOf(o.Channel, "cli", "hosted") || !oneReviewOf(o.Status, "", "pending", "all") {
		return validation()
	}
	if _, err := pageQuery(o.PageOptions); err != nil {
		return err
	}
	if o.Cursor != "" && !validOpaqueCursor(o.Cursor) {
		return validation()
	}
	return nil
}

// ReviewOperations discovers compact retained operations without provider work
// or automatic pagination. A page cursor preserves filters, not mutable state.
func (c *Client) ReviewOperations(ctx context.Context, options ReviewOperationsOptions) (ReviewOperationsPage, error) {
	if err := ValidateReviewOperationsOptions(options); err != nil {
		return ReviewOperationsPage{}, err
	}
	if options.Limit == 0 {
		options.Limit = 5
	}
	if options.Status == "" {
		options.Status = "pending"
	}
	query, _ := pageQuery(options.PageOptions)
	query.Set("status", options.Status)
	if options.PullNumber != 0 {
		query.Set("pull_number", strconv.FormatInt(options.PullNumber, 10))
	}
	if options.Channel != "" {
		query.Set("channel", options.Channel)
	}
	var value ReviewOperationsPage
	if err := c.get(ctx, c.projectPath("reviews", "operations"), query, true, &value); err != nil {
		return ReviewOperationsPage{}, err
	}
	if value.ProjectID != c.identity.ProjectID || !validPage(Page[ReviewOperationSummary]{Items: value.Items, Cursor: value.Cursor, HasMore: value.HasMore}, options.PageOptions) || value.Cursor != "" && !validOpaqueCursor(value.Cursor) {
		return ReviewOperationsPage{}, invalidResponse()
	}
	seen := make(map[string]bool, len(value.Items))
	for i, item := range value.Items {
		if !validReviewOperationSummary(item) || seen[item.ID] || options.PullNumber != 0 && item.PullNumber != options.PullNumber || options.Channel != "" && item.Channel != options.Channel || options.Status == "pending" && !oneReviewOf(item.State, "queued", "preparing", "posting", "active", "uncertain", "held") {
			return ReviewOperationsPage{}, invalidResponse()
		}
		if i > 0 {
			prior := value.Items[i-1]
			if prior.CreatedAt < item.CreatedAt || prior.CreatedAt == item.CreatedAt && prior.ID <= item.ID {
				return ReviewOperationsPage{}, invalidResponse()
			}
		}
		seen[item.ID] = true
	}
	return value, nil
}

func validReviewOperationSummary(o ReviewOperationSummary) bool {
	return ValidateReviewOperationID(o.ID) == nil && validPullNumber(o.PullNumber) && oneReviewOf(o.Channel, "cli", "hosted") && controlHex(o.HeadSHA, 40) && (o.WorkerID == "" || validExecutionIdentifier(o.WorkerID)) && oneReviewOf(o.State, "queued", "preparing", "posting", "active", "uncertain", "held", "completed", "failed", "cancelled", "rate_limited") && (o.Reason == "" || validExecutionIdentifier(o.Reason)) && (o.AttemptID == "" || validReviewOpaqueID(o.AttemptID, "review_attempt_")) && o.Revision >= 1 && controlTime(o.CreatedAt) && controlTime(o.UpdatedAt)
}
