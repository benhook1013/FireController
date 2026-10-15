package client

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// ReviewSourceReviewStateSummary deliberately omits provider thread inventories.
type ReviewSourceReviewStateSummary struct {
	State              string  `json:"state"`
	Reason             string  `json:"reason"`
	ObservedAt         string  `json:"observed_at"`
	PullState          string  `json:"pull_state"`
	Draft              bool    `json:"draft"`
	Mergeable          string  `json:"mergeable"`
	MergeStateStatus   string  `json:"merge_state_status"`
	ReviewDecision     *string `json:"review_decision"`
	TotalThreads       int64   `json:"total_threads"`
	UnresolvedCurrent  int64   `json:"unresolved_current"`
	UnresolvedOutdated int64   `json:"unresolved_outdated"`
}

// ReviewSourceReadinessAssessment describes retained observations and local
// policy as of their timestamps. It is never an authorization to merge.
type ReviewSourceReadinessAssessment struct {
	ProjectID              string                          `json:"project_id"`
	RefreshID              string                          `json:"refresh_id"`
	Source                 string                          `json:"source"`
	ConfigurationRevision  int64                           `json:"configuration_revision"`
	PolicyRevision         int64                           `json:"policy_revision"`
	SourceObservedAt       string                          `json:"source_observed_at"`
	EvaluatedAt            string                          `json:"evaluated_at"`
	Target                 ReviewSourceObservation         `json:"target"`
	State                  string                          `json:"state"`
	Reasons                []string                        `json:"reasons"`
	Channels               []ReviewPolicyAssessmentChannel `json:"channels"`
	IncomingRoutesPending  int64                           `json:"incoming_routes_pending,omitempty"`
	IncomingRoutesCapacity bool                            `json:"incoming_routes_capacity,omitempty"`
	CI                     ReviewSourceCISummary           `json:"ci"`
	ReviewState            ReviewSourceReviewStateSummary  `json:"review_state"`
}

func validSourceReviewState(value ReviewSourceReviewStateSummary) bool {
	if !controlTime(value.ObservedAt) || value.TotalThreads < 0 || value.TotalThreads > 10000 || value.UnresolvedCurrent < 0 || value.UnresolvedOutdated < 0 || value.UnresolvedCurrent > value.TotalThreads-value.UnresolvedOutdated {
		return false
	}
	if value.State == "unproven" {
		return validExecutionIdentifier(value.Reason) && value.PullState == "" && !value.Draft && value.Mergeable == "" && value.MergeStateStatus == "" && value.ReviewDecision == nil && value.TotalThreads == 0 && value.UnresolvedCurrent == 0 && value.UnresolvedOutdated == 0
	}
	return value.State == "complete" && value.Reason == "" && oneReviewOf(value.PullState, "OPEN", "CLOSED", "MERGED") && oneReviewOf(value.Mergeable, "MERGEABLE", "CONFLICTING", "UNKNOWN") && oneReviewOf(value.MergeStateStatus, "BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE") && (value.ReviewDecision == nil || oneReviewOf(*value.ReviewDecision, "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"))
}

func reviewStateReady(value ReviewSourceReviewStateSummary) bool {
	return value.State == "complete" && value.PullState == "OPEN" && !value.Draft && value.Mergeable == "MERGEABLE" && value.MergeStateStatus == "CLEAN" && (value.ReviewDecision == nil || *value.ReviewDecision == "APPROVED") && value.UnresolvedCurrent == 0 && value.UnresolvedOutdated == 0
}

func (c *Client) ReviewSourceReadiness(ctx context.Context, number int64) (ReviewSourceReadinessAssessment, error) {
	if !validPullNumber(number) {
		return ReviewSourceReadinessAssessment{}, validation()
	}
	var value ReviewSourceReadinessAssessment
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "source", strconv.FormatInt(number, 10), "readiness"), nil, true, "", nil, http.StatusOK, false, &value, 16<<10, decodeReviewResponse); err != nil {
		return ReviewSourceReadinessAssessment{}, err
	}
	if value.ProjectID != c.identity.ProjectID || ValidateReviewSourceRefreshID(value.RefreshID) != nil || value.Source != "observed_comparison" || value.ConfigurationRevision < 0 || value.PolicyRevision < 0 || value.SourceObservedAt != value.Target.ObservedAt || !validSourceObservation(value.Target) || value.Target.PullNumber != number || !controlTime(value.EvaluatedAt) || !oneReviewOf(value.State, "ready_as_observed", "held", "unconfigured") || value.Reasons == nil || len(value.Reasons) > 16 || value.Channels == nil || len(value.Channels) > 2 || !validCISummary(value.CI, true) || value.CI.Selection == "head" && value.CI.SelectedCommitSHA != value.Target.HeadSHA || !validSourceReviewState(value.ReviewState) || value.IncomingRoutesPending < 0 || value.IncomingRoutesPending > 100 || value.IncomingRoutesCapacity && value.IncomingRoutesPending != 0 {
		return ReviewSourceReadinessAssessment{}, invalidResponse()
	}
	observed, _ := time.Parse(time.RFC3339Nano, value.SourceObservedAt)
	evaluated, _ := time.Parse(time.RFC3339Nano, value.EvaluatedAt)
	ciAt, _ := time.Parse(time.RFC3339Nano, value.CI.ObservedAt)
	reviewAt, _ := time.Parse(time.RFC3339Nano, value.ReviewState.ObservedAt)
	if evaluated.Before(observed) || ciAt.Before(observed) || reviewAt.Before(observed) || evaluated.Before(ciAt) || evaluated.Before(reviewAt) {
		return ReviewSourceReadinessAssessment{}, invalidResponse()
	}
	reasons := map[string]bool{}
	for _, reason := range value.Reasons {
		if !validExecutionIdentifier(reason) || reasons[reason] {
			return ReviewSourceReadinessAssessment{}, invalidResponse()
		}
		reasons[reason] = true
	}
	covered := len(value.Channels) > 0
	channels := map[string]bool{}
	for _, channel := range value.Channels {
		if !oneReviewOf(channel.Channel, "cli", "hosted") || channels[channel.Channel] {
			return ReviewSourceReadinessAssessment{}, invalidResponse()
		}
		channels[channel.Channel] = true
		covered = covered && channel.Covered && !channel.Pending
	}
	factsReady := value.ConfigurationRevision > 0 && value.PolicyRevision > 0 && covered && value.IncomingRoutesPending == 0 && !value.IncomingRoutesCapacity && oneReviewOf(value.CI.State, "satisfied", "not_required") && value.CI.MergeState == "verified" && reviewStateReady(value.ReviewState)
	if value.State == "ready_as_observed" {
		if !factsReady || len(value.Reasons) != 0 {
			return ReviewSourceReadinessAssessment{}, invalidResponse()
		}
	} else if len(value.Reasons) == 0 {
		return ReviewSourceReadinessAssessment{}, invalidResponse()
	}
	if value.State == "unconfigured" && (value.PolicyRevision != 0 || len(value.Channels) != 0) || value.PolicyRevision == 0 && len(value.Channels) != 0 {
		return ReviewSourceReadinessAssessment{}, invalidResponse()
	}
	return value, nil
}
