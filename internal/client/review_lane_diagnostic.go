package client

import (
	"context"
	"net/http"
	"time"
)

type ReviewLanePendingSelection struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ReviewLaneDiagnostic struct {
	PendingSelection            *ReviewLanePendingSelection     `json:"pending_selection,omitempty"`
	ProjectID                   string                          `json:"project_id"`
	Channel                     string                          `json:"channel"`
	ConfigurationRevision       int64                           `json:"configuration_revision"`
	EvaluatedAt                 string                          `json:"evaluated_at"`
	Scope                       string                          `json:"scope"`
	SourceConfigurationRevision int64                           `json:"source_configuration_revision"`
	SourceCheckedAt             string                          `json:"source_checked_at,omitempty"`
	SourceComplete              bool                            `json:"source_complete"`
	SourceReason                string                          `json:"source_reason"`
	LaneReason                  string                          `json:"lane_reason"`
	Candidates                  []ReviewLaneCandidateDiagnostic `json:"candidates"`
}

type ReviewLaneCandidateDiagnostic struct {
	PullNumber         int64  `json:"pull_number"`
	HeadSHA            string `json:"head_sha,omitempty"`
	SourceObservedAt   string `json:"source_observed_at,omitempty"`
	ParentPullNumber   int64  `json:"parent_pull_number"`
	LocalState         string `json:"local_state"`
	LocalReason        string `json:"local_reason"`
	BlockingPullNumber int64  `json:"blocking_pull_number"`
	BlockingReason     string `json:"blocking_reason"`
}

func (c *Client) ReviewLaneDiagnostic(ctx context.Context, channel string) (ReviewLaneDiagnostic, error) {
	if !oneReviewOf(channel, "cli", "hosted") {
		return ReviewLaneDiagnostic{}, validation()
	}
	var out ReviewLaneDiagnostic
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "lanes", channel, "diagnostics"), nil, true, "", nil, http.StatusOK, false, &out, 128<<10, decodeReviewResponse); err != nil {
		return ReviewLaneDiagnostic{}, err
	}
	if out.ProjectID != c.identity.ProjectID || out.Channel != channel || out.Scope != "retained_source_local_policy" || out.ConfigurationRevision < 1 || !controlTime(out.EvaluatedAt) || out.SourceConfigurationRevision < 0 || out.Candidates == nil || len(out.Candidates) > 100 || out.SourceReason != "" && !safeCode(out.SourceReason) || out.LaneReason != "" && !safeCode(out.LaneReason) {
		return ReviewLaneDiagnostic{}, invalidResponse()
	}
	if out.SourceConfigurationRevision == 0 {
		if out.SourceCheckedAt != "" || out.SourceComplete || out.SourceReason != "source_unavailable" {
			return ReviewLaneDiagnostic{}, invalidResponse()
		}
	} else if !controlTime(out.SourceCheckedAt) || out.SourceComplete && (out.SourceConfigurationRevision != out.ConfigurationRevision || out.SourceReason != "") {
		return ReviewLaneDiagnostic{}, invalidResponse()
	}
	if pending := out.PendingSelection; pending != nil {
		created, _ := time.Parse(time.RFC3339Nano, pending.CreatedAt)
		updated, _ := time.Parse(time.RFC3339Nano, pending.UpdatedAt)
		if channel != "hosted" || ValidateReviewRunSelectionID(pending.ID) != nil || !oneReviewOf(pending.State, "queued", "selecting") || !controlTime(pending.CreatedAt) || !controlTime(pending.UpdatedAt) || updated.Before(created) {
			return ReviewLaneDiagnostic{}, invalidResponse()
		}
	}
	usableSource := out.SourceConfigurationRevision > 0 && out.SourceConfigurationRevision == out.ConfigurationRevision && out.SourceReason != "repository_changed" && out.SourceReason != "configuration_changed"
	seen := map[int64]bool{}
	for _, p := range out.Candidates {
		if !validPullNumber(p.PullNumber) || seen[p.PullNumber] || !oneReviewOf(p.LocalState, "unknown", "held", "complete", "locally_ready") || p.LocalReason != "" && !safeCode(p.LocalReason) || p.BlockingReason != "" && !safeCode(p.BlockingReason) || p.ParentPullNumber != 0 && (!validPullNumber(p.ParentPullNumber) || p.ParentPullNumber == p.PullNumber) || p.BlockingPullNumber != 0 && !validPullNumber(p.BlockingPullNumber) {
			return ReviewLaneDiagnostic{}, invalidResponse()
		}
		seen[p.PullNumber] = true
		if !usableSource && p.HeadSHA != "" || out.SourceComplete && p.HeadSHA == "" || p.LocalState == "unknown" && p.LocalReason == "" || p.LocalState == "complete" && p.LocalReason != "" {
			return ReviewLaneDiagnostic{}, invalidResponse()
		}
		// Selection can retain a source-free paused placeholder while it scans
		// later candidates. Only that exact incomplete-frame shape is trusted.
		pausedPlaceholder := usableSource && !out.SourceComplete && p.LocalState == "held" && p.LocalReason == "pull_scheduling_paused" && p.HeadSHA == "" && p.SourceObservedAt == "" && p.ParentPullNumber == 0 && p.BlockingPullNumber == 0 && p.BlockingReason == ""
		if (p.HeadSHA == "") != (p.SourceObservedAt == "") || p.HeadSHA != "" && (!controlHex(p.HeadSHA, 40) || !controlTime(p.SourceObservedAt)) || p.HeadSHA == "" && !pausedPlaceholder && (p.LocalState != "unknown" || p.ParentPullNumber != 0) || p.LocalState == "held" && p.LocalReason == "" || p.LocalState == "locally_ready" && p.LocalReason != "" || (p.BlockingPullNumber == 0) != (p.BlockingReason == "") || p.BlockingPullNumber != 0 && (p.LocalState != "held" || p.LocalReason != "predecessor_unfinished") {
			return ReviewLaneDiagnostic{}, invalidResponse()
		}
	}
	return out, nil
}
