package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type ReviewSelectionCandidateDecision struct {
	PullNumber         int64  `json:"pull_number"`
	HeadSHA            string `json:"head_sha,omitempty"`
	Outcome            string `json:"outcome"`
	Reason             string `json:"reason,omitempty"`
	BlockingPullNumber int64  `json:"blocking_pull_number,omitempty"`
}
type ReviewSelectionDecision struct {
	Sequence           int64                              `json:"sequence"`
	Phase              string                             `json:"phase"`
	SelectionID        string                             `json:"selection_id,omitempty"`
	Outcome            string                             `json:"outcome"`
	Reason             string                             `json:"reason,omitempty"`
	SelectedPullNumber int64                              `json:"selected_pull_number,omitempty"`
	Candidates         []ReviewSelectionCandidateDecision `json:"candidates"`
	FirstSeenAt        string                             `json:"first_seen_at"`
	LastSeenAt         string                             `json:"last_seen_at"`
	Occurrences        int64                              `json:"occurrences"`
}
type ReviewSelectionDecisionHistory struct {
	OmittedRecentPasses int                       `json:"omitted_recent_passes"`
	ProjectID           string                    `json:"project_id"`
	Channel             string                    `json:"channel"`
	RetentionDays       int                       `json:"retention_days"`
	MaximumEntries      int                       `json:"maximum_entries"`
	Items               []ReviewSelectionDecision `json:"items"`
	HasMore             bool                      `json:"has_more"`
}

func (c *Client) ReviewSelectionDecisionHistory(ctx context.Context, channel string, before int64, limit int) (ReviewSelectionDecisionHistory, error) {
	var out ReviewSelectionDecisionHistory
	if !oneReviewOf(channel, "cli", "hosted") || before < 0 || limit < 1 || limit > 100 {
		return out, validation()
	}
	path := c.projectPath("reviews", "lanes", channel, "decisions")
	query := url.Values{"limit": []string{strconv.Itoa(limit)}}
	if before > 0 {
		query.Set("before", strconv.FormatInt(before, 10))
	}
	if err := c.requestBounded(ctx, http.MethodGet, path, query, true, "", nil, http.StatusOK, false, &out, 7<<20, decodeReviewResponse); err != nil {
		return out, err
	}
	if out.OmittedRecentPasses < 0 || out.OmittedRecentPasses > 256 {
		return out, validation()
	}
	if out.ProjectID != c.identity.ProjectID || out.Channel != channel || out.RetentionDays != 7 || out.MaximumEntries != 256 || out.Items == nil || len(out.Items) > limit || out.HasMore && len(out.Items) != limit {
		return out, invalidResponse()
	}
	previous := before
	for _, d := range out.Items {
		if d.Sequence < 1 || previous > 0 && d.Sequence >= previous || !oneReviewOf(d.Phase, "selection", "automatic_plan") || !safeCode(d.Outcome) || d.Reason != "" && !safeCode(d.Reason) || d.SelectionID != "" && ValidateReviewRunSelectionID(d.SelectionID) != nil || d.SelectedPullNumber != 0 && !validPullNumber(d.SelectedPullNumber) || !controlTime(d.FirstSeenAt) || !controlTime(d.LastSeenAt) || d.Occurrences < 1 || d.Candidates == nil || len(d.Candidates) > 100 {
			return out, invalidResponse()
		}
		first, _ := time.Parse(time.RFC3339Nano, d.FirstSeenAt)
		last, _ := time.Parse(time.RFC3339Nano, d.LastSeenAt)
		if last.Before(first) || d.Phase == "selection" && d.SelectionID == "" || d.Phase == "automatic_plan" && (d.SelectionID != "" || d.SelectedPullNumber != 0) || (d.Outcome == "selected") != (d.SelectedPullNumber != 0) {
			return out, invalidResponse()
		}
		previous = d.Sequence
		seen := map[int64]bool{}
		for _, p := range d.Candidates {
			if !validPullNumber(p.PullNumber) || seen[p.PullNumber] || p.HeadSHA != "" && !controlHex(p.HeadSHA, 40) || !safeCode(p.Outcome) || p.Reason != "" && !safeCode(p.Reason) || p.BlockingPullNumber != 0 && !validPullNumber(p.BlockingPullNumber) {
				return out, invalidResponse()
			}
			seen[p.PullNumber] = true
		}
	}
	return out, nil
}
