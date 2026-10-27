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

func TestAgentOperationStatusOnlySoftensKnownScheduledObservation(t *testing.T) {
	base := ReviewOperationStatus{ID: "original", Channel: "hosted", State: "uncertain", Stage: "uncertain", Reason: "hosted_observation_pending", AttemptID: "original_attempt", Revision: 5, UpdatedAt: "original_time", NextAction: "contact_controller_owner"}
	for _, change := range []func(*ReviewOperationStatus){nil,
		func(v *ReviewOperationStatus) { v.Channel = "cli" },
		func(v *ReviewOperationStatus) { v.Reason = "trigger_post_unproven" },
		func(v *ReviewOperationStatus) { v.Reason = "hosted_observation_conflict" },
		func(v *ReviewOperationStatus) {
			v.Reason = "hosted_finished_inventory_unproven"
			v.Stage = "capture_blocked"
		},
		func(v *ReviewOperationStatus) { v.State = "active"; v.Stage = "provider_wait"; v.NextAction = "wait" },
		func(v *ReviewOperationStatus) { v.AttemptID = "" },
	} {
		value := base
		if change != nil {
			change(&value)
		}
		want := value
		if change == nil {
			want.NextAction = "wait_for_observation"
		}
		if got := AgentReviewOperationStatus(value); got.ReviewOperationStatus != want || got.Diagnostic != nil {
			t.Fatal("guidance changed retained facts or unrelated uncertainty", got, want)
		}
	}
}

func TestAgentOperationParentDiagnosticsPreserveCompactAndFullFacts(t *testing.T) {
	for _, state := range []string{"held", "queued", "preparing", "active", "completed", "failed", "uncertain", "rate_limited", "cancelled"} {
		for _, reason := range []string{"parent_moved", "predecessor_parent_moved", "parent_unavailable", "other_reason"} {
			compact := ReviewOperationStatus{ID: "original_operation", PullNumber: 3300, Channel: "hosted", State: state, Revision: 5, Stage: "held", Reason: reason, AttemptID: "original_attempt", EvidenceID: "original_evidence", UpdatedAt: "original_time", NextAction: "read_diagnostics"}
			full := ReviewOperation{ID: compact.ID, ProjectID: "original_project", ActorID: "original_actor", WorkerID: "original_worker", ConfigurationRevision: 12, PolicyRevision: 2, CapabilityFingerprint: "original_binding", PullNumber: compact.PullNumber, Channel: compact.Channel, HeadSHA: "original_head", AllowOutOfOrder: true, State: state, Revision: compact.Revision, Reason: reason, AttemptID: compact.AttemptID, CreatedAt: "original_creation", UpdatedAt: compact.UpdatedAt}
			compactOut, fullOut := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
			wantHint := state == "held" && (reason == "parent_moved" || reason == "predecessor_parent_moved")
			if !reflect.DeepEqual(compactOut.ReviewOperationStatus, compact) || !reflect.DeepEqual(fullOut.ReviewOperation, full) || !reflect.DeepEqual(compactOut.Diagnostic, fullOut.Diagnostic) || (compactOut.Diagnostic != nil) != wantHint {
				t.Fatal("parent diagnostic changed facts, disagreed across views or invented a hold", state, reason)
			}
			if wantHint {
				hint := compactOut.Diagnostic
				if reason == "parent_moved" && (!strings.Contains(hint.Explanation, "Older operations") || !strings.Contains(hint.Explanation, "does not identify")) || reason == "predecessor_parent_moved" && (!strings.Contains(hint.Explanation, "selected PR's source check passed") || !strings.Contains(hint.Explanation, "ancestor")) {
					t.Fatal("parent diagnosis invented an edge or lost the proved boundary", hint)
				}
				for _, required := range []string{"owning workflow", "stable selected comparison", "current review authorization", "do not automatically retry", "reset review progress", "no-op commit"} {
					if !strings.Contains(hint.NextAction, required) {
						t.Fatal("unsafe or incomplete parent repair guidance", hint)
					}
				}
			}
			for _, value := range []struct{ raw, output any }{{compact, compactOut}, {full, fullOut}} {
				body, err := json.Marshal(value.output)
				if err != nil || len(body) > 2048 {
					t.Fatal("unbounded diagnostic", err)
				}
				var got, want map[string]json.RawMessage
				if json.Unmarshal(body, &got) != nil || (got["diagnostic"] != nil) != wantHint {
					t.Fatal("missing or invented parent hint", string(body))
				}
				delete(got, "diagnostic")
				original, _ := json.Marshal(value.raw)
				_ = json.Unmarshal(original, &want)
				if !reflect.DeepEqual(got, want) {
					t.Fatal("agent output changed retained operation JSON", string(body))
				}
			}
		}
	}
}
func TestAgentCIDiagnosticPreservesRefusal(t *testing.T) {
	for _, reason := range []string{"ci_merge_identity_unproven", "ci_merge_base_mismatch", "ci_merge_pending", "ci_provider_observation_changed_assessment", "ci_observation_changed", "ci_provider_observation_changed_head_results", "ci_provider_observation_changed_merge_results", "ci_provider_observation_changed_results_recheck", "ci_provider_observation_changed_merge_results_recheck", "ci_provider_invalid_response_assessment", "other_reason"} {
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
		for _, reason := range []string{"ci_merge_base_mismatch", "ci_provider_observation_changed_assessment", "ci_provider_observation_changed_head_results", "ci_provider_invalid_response_assessment", "unknown"} {
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

func TestAgentAllowanceDiagnosticsKeepOriginalStatusAndDoNotAuthorizeRetry(t *testing.T) {
	for _, reason := range []string{"hosted_allowance_reply_pending", "hosted_allowance_unproven"} {
		v := ReviewOperationStatus{ID: "original", Channel: "hosted", State: "held", Reason: reason, Stage: "held", NextAction: "read_diagnostics", AttemptID: "original_attempt", Revision: 3}
		out := AgentReviewOperationStatus(v)
		full := ReviewOperation{ID: v.ID, Channel: v.Channel, State: v.State, Reason: v.Reason, AttemptID: v.AttemptID, Revision: v.Revision}
		if !reflect.DeepEqual(out.ReviewOperationStatus, v) || out.Diagnostic == nil || !reflect.DeepEqual(out.Diagnostic, AgentReviewOperation(full).Diagnostic) || !strings.Contains(strings.ToLower(out.Diagnostic.NextAction), "do not") {
			t.Fatal("missing safe guidance or changed original operation", out)
		}
		v.State = "active"
		if AgentReviewOperationStatus(v).Diagnostic != nil {
			t.Fatal("held diagnosis invented for active work")
		}
	}
}
