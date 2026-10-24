package client

import (
	"context"
	"net/http"
)

// ReviewOperationStatus omits private configuration and capture contents.
type ReviewOperationStatus struct {
	ID         string `json:"id"`
	PullNumber int64  `json:"pull_number"`
	Channel    string `json:"channel"`
	State      string `json:"state"`
	Revision   int64  `json:"revision"`
	Stage      string `json:"stage"`
	Reason     string `json:"reason"`
	AttemptID  string `json:"attempt_id"`
	EvidenceID string `json:"evidence_id,omitempty"`
	UpdatedAt  string `json:"updated_at"`
	NextAction string `json:"next_action"`
}

func (c *Client) ReviewOperationStatus(ctx context.Context, id string) (ReviewOperationStatus, error) {
	return c.WaitReviewOperationStatus(ctx, id, 0)
}
func (c *Client) WaitReviewOperationStatus(ctx context.Context, id string, seconds int) (ReviewOperationStatus, error) {
	if ValidateReviewOperationID(id) != nil || ValidateReviewOperationWait(seconds) != nil {
		return ReviewOperationStatus{}, validation()
	}
	var value ReviewOperationStatus
	var status int
	if err := c.requestBoundedOptions(ctx, http.MethodGet, c.projectPath("reviews", "operations", id, "status"), nil, true, "", nil, http.StatusOK, false, &value, 16<<10, decodeReviewResponse, &sourceWaitOptions{seconds: seconds, status: &status}); err != nil {
		return ReviewOperationStatus{}, err
	}
	if value.ID != id || !validCompactOperationStatus(value) {
		return ReviewOperationStatus{}, invalidResponse()
	}
	return value, nil
}
func validCompactOperationStatus(v ReviewOperationStatus) bool {
	if ValidateReviewOperationID(v.ID) != nil || !validPullNumber(v.PullNumber) || !oneReviewOf(v.Channel, "cli", "hosted") || v.Revision < 1 || v.Reason != "" && !validExecutionIdentifier(v.Reason) || v.AttemptID != "" && !validReviewOpaqueID(v.AttemptID, "review_attempt_") || v.EvidenceID != "" && (!validReviewOpaqueID(v.EvidenceID, "review_evidence_") || v.AttemptID == "") || !controlTime(v.UpdatedAt) {
		return false
	}
	switch v.State {
	case "queued":
		return v.Stage == "queued" && v.NextAction == "wait"
	case "preparing":
		return v.Stage == "preparing" && v.NextAction == "wait"
	case "posting", "active":
		return (v.Stage == "provider_wait" || v.Stage == "result_captured" && v.EvidenceID != "") && v.NextAction == "wait"
	case "completed":
		return v.EvidenceID != "" && (v.Stage == "evidence_ready" && v.NextAction == "read_evidence" || v.Stage == "capture_blocked" && v.NextAction == "contact_controller_owner") || v.EvidenceID == "" && v.Stage == "unknown" && v.NextAction == "contact_controller_owner"
	case "uncertain":
		if v.Channel == "hosted" && v.Reason == "hosted_finished_inventory_unproven" {
			return v.Stage == "capture_blocked" && v.NextAction == "contact_controller_owner"
		}
		return v.Stage == "uncertain" && v.NextAction == "contact_controller_owner"
	case "held", "rate_limited":
		return v.Stage == "held" && v.NextAction == "read_diagnostics"
	case "failed":
		return v.Stage == "failed" && v.NextAction == "read_diagnostics"
	case "cancelled":
		return v.Stage == "cancelled" && v.NextAction == "none"
	}
	return false
}
