package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func pendingEvidenceSample() *ReviewPendingEvidence {
	finding := int64(0)
	return &ReviewPendingEvidence{EvaluatedAt: "2026-10-29T03:00:00Z", Items: []ReviewPendingEvidenceItem{{OperationID: "review_operation_" + strings.Repeat("a", 32), EvidenceID: "review_evidence_" + strings.Repeat("b", 32), Finding: &finding, RouteID: "review_route_" + strings.Repeat("c", 32), TargetPullNumber: 42, Reason: "route_fix_pending"}}}
}
func TestPendingEvidenceValidationAndAgentOutput(t *testing.T) {
	v := pendingEvidenceSample()
	if !validReviewPendingEvidence(v, "held", "review_evidence_pending") {
		t.Fatal("valid blocker refused")
	}
	for _, change := range []func(*ReviewPendingEvidence){
		func(v *ReviewPendingEvidence) { v.EvaluatedAt = "unknown" }, func(v *ReviewPendingEvidence) { v.Items = nil },
		func(v *ReviewPendingEvidence) { v.HasMore = true }, func(v *ReviewPendingEvidence) { v.Items = append(v.Items, v.Items[0]) },
		func(v *ReviewPendingEvidence) { v.Items[0].OperationID = "private" }, func(v *ReviewPendingEvidence) { v.Items[0].Finding = nil },
		func(v *ReviewPendingEvidence) { v.Items[0].RouteID = "review_evidence_" + strings.Repeat("a", 32) },
		func(v *ReviewPendingEvidence) { v.Items[0].TargetPullNumber = 0 }, func(v *ReviewPendingEvidence) { v.Items[0].Reason = "future_unknown" },
	} {
		copy := pendingEvidenceSample()
		change(copy)
		if validReviewPendingEvidence(copy, "held", "review_evidence_pending") {
			t.Fatal("invalid blocker accepted", copy)
		}
	}
	if validReviewPendingEvidence(v, "active", "review_evidence_pending") || validReviewPendingEvidence(v, "held", "allowance_timeout") {
		t.Fatal("blockers accepted for wrong state")
	}
	compact := ReviewOperationStatus{ID: "original", PullNumber: 2, Channel: "cli", State: "held", Stage: "held", Reason: "review_evidence_pending", Revision: 3, NextAction: "read_diagnostics", PendingEvidence: v}
	full := ReviewOperation{ID: compact.ID, State: compact.State, Reason: compact.Reason, Channel: compact.Channel, Revision: compact.Revision, PendingEvidence: v}
	a, b := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
	want := compact
	want.NextAction = "resolve_pending_evidence"
	if !reflect.DeepEqual(a.ReviewOperationStatus, want) || !reflect.DeepEqual(b.ReviewOperation, full) || !reflect.DeepEqual(a.Diagnostic, b.Diagnostic) {
		t.Fatal("guidance changed original facts")
	}
	if !strings.Contains(a.Diagnostic.Explanation, "current local") || !strings.Contains(a.Diagnostic.NextAction, "terminal") {
		t.Fatal("blocker read implied historical capture or resume")
	}
	encoded, _ := json.Marshal(a)
	if len(encoded) > 4096 {
		t.Fatal("blocker result too large")
	}
	v.Items = []ReviewPendingEvidenceItem{}
	if !validReviewPendingEvidence(v, "held", "review_evidence_pending") || AgentReviewOperationStatus(compact).NextAction != "new_request_if_authorized" {
		t.Fatal("cleared current obligations did not preserve terminal hold")
	}
}
