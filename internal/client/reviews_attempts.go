package client

import (
	"context"
	"net/http"
	"strconv"
)

// ReviewAttemptOptions selects one explicit, snapshot-bound attempt page.
type ReviewAttemptOptions struct {
	PageOptions
	Snapshot string
}

// ReviewAttemptItem retains the original producer, independently of the publisher.
type ReviewAttemptItem struct {
	ReviewAttempt
	Source string `json:"source"`
}

type ReviewAttempts struct {
	ProjectID  string              `json:"project_id"`
	Revision   int64               `json:"revision"`
	SnapshotID string              `json:"snapshot_id"`
	Source     string              `json:"source"`
	ObservedAt string              `json:"observed_at"`
	RecordedAt string              `json:"recorded_at"`
	Items      []ReviewAttemptItem `json:"items"`
	Cursor     string              `json:"cursor,omitempty"`
	HasMore    bool                `json:"has_more"`
}

// Flat wire fields keep the shared strict decoder authoritative: it does not
// flatten anonymous Go structs. Public callers still use ReviewAttemptItem.
type reviewAttemptWire struct {
	Source           string `json:"source"`
	SourceAttemptID  string `json:"source_attempt_id"`
	Channel          string `json:"channel"`
	HeadSHA          string `json:"head_sha"`
	State            string `json:"state"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
	DurationSeconds  *int64 `json:"duration_seconds"`
	SourceRunID      string `json:"source_run_id"`
	TriggerID        string `json:"trigger_id"`
	ProviderReviewID string `json:"provider_review_id"`
}

type reviewAttemptsWire struct {
	ProjectID  string              `json:"project_id"`
	Revision   int64               `json:"revision"`
	SnapshotID string              `json:"snapshot_id"`
	Source     string              `json:"source"`
	ObservedAt string              `json:"observed_at"`
	RecordedAt string              `json:"recorded_at"`
	Items      []reviewAttemptWire `json:"items"`
	Cursor     string              `json:"cursor,omitempty"`
	HasMore    bool                `json:"has_more"`
}

// ReviewAttempts reads one newest-first page, without draining its continuation.
func (c *Client) ReviewAttempts(ctx context.Context, number int64, options ReviewAttemptOptions) (ReviewAttempts, error) {
	query, err := reviewPageQuery(options.PageOptions)
	if err != nil || !validPullNumber(number) || options.Snapshot != "" && !validReviewSnapshot(options.Snapshot) {
		return ReviewAttempts{}, validation()
	}
	if options.Snapshot != "" {
		query.Set("snapshot", options.Snapshot)
	}
	var wire reviewAttemptsWire
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "pulls", strconv.FormatInt(number, 10), "attempts"), query, true, "", nil, http.StatusOK, false, &wire, maxReviewListResponse, decodeReviewResponse); err != nil {
		return ReviewAttempts{}, err
	}
	if !c.validReviewMeta(wire.ProjectID, wire.Revision, wire.SnapshotID, wire.Source, wire.ObservedAt, wire.RecordedAt, true) || options.Snapshot != "" && wire.SnapshotID != options.Snapshot || !validReviewPage(wire.Items, wire.Cursor, wire.HasMore, options.PageOptions) || len(wire.Items) > maxReviewPageItems {
		return ReviewAttempts{}, invalidResponse()
	}
	out := ReviewAttempts{ProjectID: wire.ProjectID, Revision: wire.Revision, SnapshotID: wire.SnapshotID, Source: wire.Source, ObservedAt: wire.ObservedAt, RecordedAt: wire.RecordedAt, Items: make([]ReviewAttemptItem, 0, len(wire.Items)), Cursor: wire.Cursor, HasMore: wire.HasMore}
	seen := make(map[[2]string]struct{}, len(wire.Items))
	for _, item := range wire.Items {
		attempt := ReviewAttempt{SourceAttemptID: item.SourceAttemptID, Channel: item.Channel, HeadSHA: item.HeadSHA, State: item.State, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt, DurationSeconds: item.DurationSeconds, SourceRunID: item.SourceRunID, TriggerID: item.TriggerID, ProviderReviewID: item.ProviderReviewID}
		if !cleanReviewText(item.Source, 128, true) || !validReviewAttempt(attempt) {
			return ReviewAttempts{}, invalidResponse()
		}
		identity := [2]string{item.Source, item.SourceAttemptID}
		if _, exists := seen[identity]; exists {
			return ReviewAttempts{}, invalidResponse()
		}
		seen[identity] = struct{}{}
		out.Items = append(out.Items, ReviewAttemptItem{ReviewAttempt: attempt, Source: item.Source})
	}
	return out, nil
}
