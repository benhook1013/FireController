package client

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// ReviewOperationStatus omits private configuration and capture contents.
type ReviewOperationStatus struct {
	SettlementTarget   *ReviewSettlementTarget     `json:"settlement_target,omitempty"`
	Observer           *ReviewHostedObserverStatus `json:"observer,omitempty"`
	Wait               *ReviewOperationWait        `json:"wait,omitempty"`
	SourceRefusal      *ReviewSourceRefusal        `json:"source_refusal,omitempty"`
	deploymentDraining bool
	PendingEvidence    *ReviewPendingEvidence `json:"pending_evidence,omitempty"`
	ID                 string                 `json:"id"`
	PullNumber         int64                  `json:"pull_number"`
	Channel            string                 `json:"channel"`
	State              string                 `json:"state"`
	Revision           int64                  `json:"revision"`
	Stage              string                 `json:"stage"`
	Reason             string                 `json:"reason"`
	AttemptID          string                 `json:"attempt_id"`
	EvidenceID         string                 `json:"evidence_id,omitempty"`
	ReviewFinishedAt   string                 `json:"review_finished_at,omitempty"`
	EvidenceRecordedAt string                 `json:"evidence_recorded_at,omitempty"`
	RetryNotBefore     string                 `json:"retry_not_before,omitempty"`
	UpdatedAt          string                 `json:"updated_at"`
	NextAction         string                 `json:"next_action"`
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
	if err := c.requestBoundedOptions(ctx, http.MethodGet, c.projectPath("reviews", "operations", id, "status"), nil, true, "", nil, http.StatusOK, false, &value, 16<<10, decodeCompactOperationStatus, &sourceWaitOptions{seconds: seconds, status: &status, reviewBlockers: true, sourceRefusalV3: true, operationWait: true, hostedObserver: true, deploymentDraining: &value.deploymentDraining}); err != nil {
		return ReviewOperationStatus{}, err
	}
	if value.ID != id || !validCompactOperationStatus(value) {
		return ReviewOperationStatus{}, invalidResponse()
	}
	return value, nil
}

// Compact status metadata can grow without changing its existing state/action
// contract. Ignore unknown additions rather than forwarding them, while keeping
// canonical required fields, duplicate-key and known-value validation strict.
func decodeCompactOperationStatus(data []byte, dst any) error {
	if err := strictjson.Validate(data); err != nil {
		return err
	}
	if !reviewShape(data, reflect.TypeOf(dst)) {
		return strictjson.ErrSyntax
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, present := fields["wait"]; present {
		var wait ReviewOperationWait
		if strictjson.Decode(raw, &wait) != nil {
			return strictjson.ErrSyntax
		}
	}
	if raw, present := fields["observer"]; present {
		var observer ReviewHostedObserverStatus
		if strictjson.Decode(raw, &observer) != nil {
			return strictjson.ErrSyntax
		}
	}
	if raw, present := fields["source_refusal"]; present {
		var refusal ReviewSourceRefusal
		if strictjson.Decode(raw, &refusal) != nil {
			return strictjson.ErrSyntax
		}
	}
	if raw, present := fields["settlement_target"]; present {
		var target ReviewSettlementTarget
		if strictjson.Decode(raw, &target) != nil {
			return strictjson.ErrSyntax
		}
	}
	typ := reflect.TypeOf(ReviewOperationStatus{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).PkgPath != "" || typ.Field(i).Tag.Get("json") == "-" {
			continue
		}
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		for supplied := range fields {
			if supplied != name && strings.EqualFold(supplied, name) {
				return strictjson.ErrSyntax
			}
		}
	}
	return json.Unmarshal(data, dst)
}

func validCompactOperationStatus(v ReviewOperationStatus) bool {
	if !validReviewSettlementTarget(v.SettlementTarget, v.AttemptID) {
		return false
	}
	if !validReviewHostedObserver(v.Observer, v.Channel, v.State, v.AttemptID) {
		return false
	}
	if !validReviewOperationWait(v.Wait, v.Channel, v.State, v.AttemptID) || !validReviewSourceRefusal(v.SourceRefusal, v.State, v.Reason, v.PullNumber) {
		return false
	}
	if !validReviewPendingEvidence(v.PendingEvidence, v.State, v.Reason) || ValidateReviewOperationID(v.ID) != nil || !validPullNumber(v.PullNumber) || !oneReviewOf(v.Channel, "cli", "hosted") || v.Revision < 1 || v.Reason != "" && !validExecutionIdentifier(v.Reason) || v.AttemptID != "" && !validReviewOpaqueID(v.AttemptID, "review_attempt_") || v.EvidenceID != "" && (!validReviewOpaqueID(v.EvidenceID, "review_evidence_") || v.AttemptID == "") || !controlTime(v.UpdatedAt) {
		return false
	}
	if (v.ReviewFinishedAt != "" || v.EvidenceRecordedAt != "") && (v.EvidenceID == "" || !controlTime(v.ReviewFinishedAt) || !controlTime(v.EvidenceRecordedAt)) {
		return false
	}
	if v.RetryNotBefore != "" && (v.Channel != "hosted" || v.State != "held" || v.Reason != "hosted_allowance_cooldown" || v.AttemptID == "" || !validAllowanceTime(v.RetryNotBefore)) {
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
