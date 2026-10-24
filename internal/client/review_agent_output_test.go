package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAgentRunOutputKeepsRunDatesAndCursor(t *testing.T) {
	v := ReviewRuns{ProjectID: "p", Revision: 7, SnapshotID: "s", Source: "old source", ObservedAt: "old origin", RecordedAt: "old save", Items: []ReviewRunSummary{{StartedAt: "new start", FinishedAt: "new finish"}}, Cursor: "next", HasMore: true}
	out := AgentReviewRuns(v)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "old ") || !strings.Contains(string(b), "new finish") || out.Cursor != v.Cursor || !out.HasMore || !reflect.DeepEqual(out.Items, v.Items) {
		t.Fatal(string(b))
	}
}
func TestAgentCIDiagnosticPreservesRefusal(t *testing.T) {
	for _, reason := range []string{"ci_merge_identity_unproven", "ci_merge_pending", "ci_provider_observation_changed_assessment", "ci_observation_changed", "ci_provider_invalid_response_assessment", "other_reason"} {
		v := ReviewSourceCIAssessment{State: "unproven", Reason: reason}
		out := AgentReviewCI(v)
		if !reflect.DeepEqual(out.ReviewSourceCIAssessment, v) || (out.Diagnostic != nil) != (reason != "other_reason") {
			t.Fatal(out)
		}
	}
	if AgentReviewCI(ReviewSourceCIAssessment{State: "held", Reason: "ci_merge_pending"}).Diagnostic != nil {
		t.Fatal("misdiagnosed local hold")
	}
}

func TestAgentCompositeCIDiagnosticsPreserveFactsAndFlatReceipts(t *testing.T) {
	for _, state := range []string{"unproven", "pending", "satisfied", "held"} {
		for _, reason := range []string{"ci_provider_observation_changed_assessment", "ci_provider_invalid_response_assessment", "unknown"} {
			ci := ReviewSourceCISummary{State: state, Reason: reason, Required: 0}
			readiness := ReviewSourceReadinessAssessment{State: "held", Reasons: []string{"ci_unproven"}, CI: ci}
			operation := ReviewSourceOperation{ID: "original", Revision: 3, State: "completed", CI: &ci}
			wantHint := state == "unproven" && reason != "unknown"
			for _, value := range []struct{ raw, output any }{
				{readiness, AgentReviewReadiness(readiness)},
				{operation, AgentReviewSourceOperation(operation)},
				{operation, AgentReviewSourceResult(ReviewSourceResult{Operation: &operation})},
			} {
				body, err := json.Marshal(value.output)
				if err != nil || len(body) > 4096 {
					t.Fatal("unbounded composite output", err)
				}
				var got, want map[string]json.RawMessage
				if json.Unmarshal(body, &got) != nil {
					t.Fatal(string(body))
				}
				if (got["ci_diagnostic"] != nil) != wantHint {
					t.Fatal("missing or invented CI diagnostic", string(body))
				}
				delete(got, "ci_diagnostic")
				original, _ := json.Marshal(value.raw)
				_ = json.Unmarshal(original, &want)
				if !reflect.DeepEqual(got, want) {
					t.Fatal("diagnostic changed retained facts", string(body))
				}
			}
		}
	}
	if AgentReviewSourceOperation(ReviewSourceOperation{State: "queued"}).CIDiagnostic != nil {
		t.Fatal("invented CI for queued operation")
	}
	receipt := &ReviewRequestReceipt{ID: "original", State: "queued", Revision: 1}
	value := ReviewSourceResult{Receipt: receipt}
	before, _ := json.Marshal(value)
	after, err := json.Marshal(AgentReviewSourceResult(value))
	if err != nil || string(before) != string(after) {
		t.Fatal("changed flat pending receipt", string(after), err)
	}
	for _, invalid := range []ReviewSourceResult{{}, {Receipt: receipt, Operation: &ReviewSourceOperation{}}} {
		if _, err := json.Marshal(AgentReviewSourceResult(invalid)); err == nil {
			t.Fatal("accepted invalid result combination")
		}
	}
}
