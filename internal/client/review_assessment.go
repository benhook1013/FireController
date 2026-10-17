package client

import (
	"context"
	"net/http"
)

// ReviewPolicyAssessment evaluates current project requirements against a
// captured comparison. It does not establish current-source or merge readiness.
type ReviewPolicyAssessment struct {
	IncomingRoutesPending  int64                           `json:"incoming_routes_pending,omitempty"`
	IncomingRoutesCapacity bool                            `json:"incoming_routes_capacity,omitempty"`
	Reason                 string                          `json:"reason,omitempty"`
	ProjectID              string                          `json:"project_id"`
	EvidenceID             string                          `json:"evidence_id"`
	Anchor                 ReviewAnchor                    `json:"anchor"`
	Source                 string                          `json:"source"`
	PolicyRevision         int64                           `json:"policy_revision"`
	EvaluatedAt            string                          `json:"evaluated_at"`
	State                  string                          `json:"state"`
	Channels               []ReviewPolicyAssessmentChannel `json:"channels"`
}

type ReviewPolicyAssessmentChannel struct {
	Channel          string `json:"channel"`
	Covered          bool   `json:"covered"`
	Pending          bool   `json:"pending"`
	RoundsConfigured bool   `json:"rounds_configured,omitempty"`
	RoundsComplete   bool   `json:"rounds_complete,omitempty"`
}

func (c *Client) ReviewPolicyAssessment(ctx context.Context, id string) (ReviewPolicyAssessment, error) {
	if err := ValidateReviewEvidenceID(id); err != nil {
		return ReviewPolicyAssessment{}, err
	}
	var value ReviewPolicyAssessment
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "evidence", id, "policy"), nil, true, "", nil, http.StatusOK, false, &value, 16<<10, decodeReviewResponse); err != nil {
		return ReviewPolicyAssessment{}, err
	}
	if value.ProjectID != c.identity.ProjectID || value.EvidenceID != id || !validControlAnchor(value.Anchor) || value.Source != "captured_comparison" || !controlTime(value.EvaluatedAt) || value.Channels == nil || len(value.Channels) > 2 {
		return ReviewPolicyAssessment{}, invalidResponse()
	}
	if !validIncomingRoutePolicy(value.IncomingRoutesPending, value.IncomingRoutesCapacity, value.Reason) {
		return ReviewPolicyAssessment{}, invalidResponse()
	}
	if value.State == "unconfigured" {
		if value.PolicyRevision != 0 || len(value.Channels) != 0 || value.Reason != "" {
			return ReviewPolicyAssessment{}, invalidResponse()
		}
		return value, nil
	}
	if value.PolicyRevision < 1 || len(value.Channels) == 0 || !oneReviewOf(value.State, "sufficient", "held") {
		return ReviewPolicyAssessment{}, invalidResponse()
	}
	seen := map[string]bool{}
	sufficient := value.IncomingRoutesPending == 0 && !value.IncomingRoutesCapacity
	for _, channel := range value.Channels {
		if !oneReviewOf(channel.Channel, "cli", "hosted") || seen[channel.Channel] || !channel.RoundsConfigured && channel.RoundsComplete {
			return ReviewPolicyAssessment{}, invalidResponse()
		}
		seen[channel.Channel] = true
		sufficient = sufficient && channel.Covered && !channel.Pending && (!channel.RoundsConfigured || channel.RoundsComplete)
	}
	if sufficient != (value.State == "sufficient") {
		return ReviewPolicyAssessment{}, invalidResponse()
	}
	return value, nil
}
