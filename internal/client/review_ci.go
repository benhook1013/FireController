package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// ReviewSourceCISummary is deliberately compact. Zero required checks while
// unproven means unknown, rather than an observed absence of requirements.
type ReviewSourceCISummary struct {
	State             string `json:"state"`
	Reason            string `json:"reason"`
	Selection         string `json:"selection"`
	SelectedCommitSHA string `json:"selected_commit_sha"`
	MergeState        string `json:"merge_state"`
	ObservedAt        string `json:"observed_at"`
	Required          int64  `json:"required"`
	Satisfied         int64  `json:"satisfied"`
	Failed            int64  `json:"failed"`
	Pending           int64  `json:"pending"`
	Unproven          int64  `json:"unproven"`
}

type ReviewSourceCIAssessment struct {
	Checks                *[]ReviewCIContext      `json:"checks,omitempty"`
	ProjectID             string                  `json:"project_id"`
	RefreshID             string                  `json:"refresh_id"`
	Source                string                  `json:"source"`
	ConfigurationRevision int64                   `json:"configuration_revision"`
	SourceObservedAt      string                  `json:"source_observed_at"`
	EvaluatedAt           string                  `json:"evaluated_at"`
	Target                ReviewSourceObservation `json:"target"`
	State                 string                  `json:"state"`
	Reason                string                  `json:"reason"`
	Selection             string                  `json:"selection"`
	SelectedCommitSHA     string                  `json:"selected_commit_sha"`
	MergeState            string                  `json:"merge_state"`
	ObservedAt            string                  `json:"observed_at"`
	Required              int64                   `json:"required"`
	Satisfied             int64                   `json:"satisfied"`
	Failed                int64                   `json:"failed"`
	Pending               int64                   `json:"pending"`
	Unproven              int64                   `json:"unproven"`
}

func validCISummary(value ReviewSourceCISummary, allowHeld bool) bool {
	if !oneReviewOf(value.State, "satisfied", "failed", "pending", "unproven", "not_required") && !(allowHeld && value.State == "held") || value.Reason != "" && !validExecutionIdentifier(value.Reason) || !controlTime(value.ObservedAt) {
		return false
	}
	if value.Selection == "" {
		if value.SelectedCommitSHA != "" || !oneReviewOf(value.State, "unproven", "not_required", "held") {
			return false
		}
	} else if !oneReviewOf(value.Selection, "head", "test_merge") || !controlHex(value.SelectedCommitSHA, 40) {
		return false
	}
	if value.MergeState != "" && !oneReviewOf(value.MergeState, "verified", "conflicting", "pending", "unproven") {
		return false
	}
	if value.Selection == "test_merge" && value.MergeState != "verified" {
		return false
	}
	counts := []int64{value.Required, value.Satisfied, value.Failed, value.Pending, value.Unproven}
	for _, count := range counts {
		if count < 0 || count > 128 {
			return false
		}
	}
	if value.Required != value.Satisfied+value.Failed+value.Pending+value.Unproven {
		return false
	}
	switch value.State {
	case "satisfied":
		return value.Required > 0 && value.Satisfied == value.Required && value.Reason == ""
	case "not_required":
		return value.Required == 0 && value.Reason == ""
	case "failed":
		return value.Failed > 0 && value.Unproven == 0 && value.Reason != ""
	case "pending":
		return value.Pending > 0 && value.Unproven == 0 && value.Failed == 0 && value.Reason != ""
	default:
		return value.Reason != ""
	}
}

func validSourceOperationCI(value ReviewSourceOperation) bool {
	if !value.IncludeCI && !value.IncludeReadiness {
		return value.CI == nil
	}
	if value.State != "completed" {
		return value.CI == nil
	}
	if value.CI == nil || !validCISummary(*value.CI, false) {
		return false
	}
	return value.CI.Selection != "head" || value.CI.SelectedCommitSHA == value.HeadSHA
}

// ReviewSourceCIAssessment reads retained facts; it never refreshes GitHub.
func (c *Client) ReviewSourceCIAssessment(ctx context.Context, number int64) (ReviewSourceCIAssessment, error) {
	return c.reviewSourceCIAssessment(ctx, number, false)
}

// ReviewSourceCIDiagnostics adds only required-context facts from retained CI.
func (c *Client) ReviewSourceCIDiagnostics(ctx context.Context, number int64) (ReviewSourceCIAssessment, error) {
	return c.reviewSourceCIAssessment(ctx, number, true)
}

func (c *Client) reviewSourceCIAssessment(ctx context.Context, number int64, diagnostics bool) (ReviewSourceCIAssessment, error) {
	if !validPullNumber(number) {
		return ReviewSourceCIAssessment{}, validation()
	}
	var value ReviewSourceCIAssessment
	var query url.Values
	budget := 16 << 10
	if diagnostics {
		query = url.Values{"diagnostics": {"true"}}
		budget = 512 << 10
	}
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "source", strconv.FormatInt(number, 10), "ci"), query, true, "", nil, http.StatusOK, false, &value, budget, decodeReviewResponse); err != nil {
		return ReviewSourceCIAssessment{}, err
	}
	if value.ProjectID != c.identity.ProjectID || ValidateReviewSourceRefreshID(value.RefreshID) != nil || value.Source != "observed_ci" || value.ConfigurationRevision < 0 || value.ConfigurationRevision == 0 && (value.State != "held" || value.Reason != "source_configuration_changed") || !validSourceObservation(value.Target) || value.Target.PullNumber != number || value.SourceObservedAt != value.Target.ObservedAt || !controlTime(value.EvaluatedAt) || !validCISummary(value.summary(), true) || value.Selection == "head" && value.SelectedCommitSHA != value.Target.HeadSHA {
		return ReviewSourceCIAssessment{}, invalidResponse()
	}
	if diagnostics != (value.Checks != nil) || !validCIContextDetails(value) {
		return ReviewSourceCIAssessment{}, invalidResponse()
	}
	return value, nil
}

func (v ReviewSourceCIAssessment) summary() ReviewSourceCISummary {
	return ReviewSourceCISummary{State: v.State, Reason: v.Reason, Selection: v.Selection, SelectedCommitSHA: v.SelectedCommitSHA, MergeState: v.MergeState, ObservedAt: v.ObservedAt, Required: v.Required, Satisfied: v.Satisfied, Failed: v.Failed, Pending: v.Pending, Unproven: v.Unproven}
}
