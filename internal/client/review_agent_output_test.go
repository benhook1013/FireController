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
	for _, reason := range []string{"ci_merge_identity_unproven", "ci_merge_base_mismatch", "ci_merge_pending", "ci_provider_observation_changed_assessment", "ci_observation_changed", "ci_provider_observation_changed_head_results", "ci_provider_observation_changed_merge_results", "ci_provider_observation_changed_results_recheck", "ci_provider_observation_changed_merge_results_recheck", "ci_provider_invalid_response_assessment", "ci_provider_invalid_response_head_results", "ci_provider_invalid_response_merge_results", "ci_provider_invalid_response_results_recheck", "ci_provider_invalid_response_merge_results_recheck", "other_reason"} {
		v := ReviewSourceCIAssessment{State: "unproven", Reason: reason}
		out := AgentReviewCI(v)
		if !reflect.DeepEqual(out.ReviewSourceCIAssessment, v) || (out.Diagnostic != nil) != (reason != "other_reason") {
			t.Fatal(out)
		}
		if reason == "ci_merge_base_mismatch" && (!strings.Contains(out.Diagnostic.Explanation, "Identical old and current base trees") || !strings.Contains(out.Diagnostic.NextAction, "normal authorized merge/rebase") || !strings.Contains(out.Diagnostic.NextAction, "does not regenerate GitHub merge refs")) {
			t.Fatal("stale-merge recovery still needs owner clarification", out)
		}
	}
	if AgentReviewCI(ReviewSourceCIAssessment{State: "held", Reason: "ci_merge_pending"}).Diagnostic != nil {
		t.Fatal("misdiagnosed local hold")
	}
}

func TestAgentCompositeCIDiagnosticsPreserveFactsAndFlatReceipts(t *testing.T) {
	for _, state := range []string{"unproven", "pending", "satisfied", "held"} {
		for _, reason := range []string{"ci_merge_base_mismatch", "ci_provider_observation_changed_assessment", "ci_provider_observation_changed_head_results", "ci_provider_invalid_response_assessment", "ci_provider_invalid_response_head_results", "unknown"} {
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
		want := v
		if reason == "hosted_allowance_reply_pending" {
			want.NextAction = "new_request_to_recheck_allowance"
		}
		if !reflect.DeepEqual(out.ReviewOperationStatus, want) || out.Diagnostic == nil || !reflect.DeepEqual(out.Diagnostic, AgentReviewOperation(full).Diagnostic) || !strings.Contains(strings.ToLower(out.Diagnostic.NextAction), "do not") {
			t.Fatal("missing safe guidance or changed original operation", out)
		}
		v.State = "active"
		if AgentReviewOperationStatus(v).Diagnostic != nil {
			t.Fatal("held diagnosis invented for active work")
		}
	}
}

func TestAgentHostedTriggerDiagnosticsDistinguishDeliveryFromExecution(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, state := range []string{"active", "posting", "uncertain", "completed", "held", "failed"} {
			for _, reason := range []string{"trigger_recovered", "trigger_recorded", "trigger_posted", "trigger_post_unproven", "hosted_observation_pending", "hosted_finished_inventory_unproven"} {
				for _, stage := range []string{"provider_wait", "result_captured"} {
					compact := ReviewOperationStatus{ID: "original", Channel: channel, State: state, Stage: stage, Reason: reason, AttemptID: "original_attempt", EvidenceID: "original_evidence", Revision: 5, UpdatedAt: "original_transition", NextAction: "wait"}
					full := ReviewOperation{ID: compact.ID, Channel: channel, State: state, Reason: reason, AttemptID: compact.AttemptID, Revision: compact.Revision, UpdatedAt: compact.UpdatedAt}
					want := channel == "hosted" && state == "active" && (reason == "trigger_recovered" || reason == "trigger_recorded" || reason == "trigger_posted")
					co, fo := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
					if co.ReviewOperationStatus != compact || !reflect.DeepEqual(fo.ReviewOperation, full) || (co.Diagnostic != nil) != want || !reflect.DeepEqual(co.Diagnostic, fo.Diagnostic) {
						t.Fatal("trigger hint changed facts, disagreed across views or misclassified state", co, fo)
					}
					if !want {
						continue
					}
					for _, phrase := range []string{"proves delivery", "not that CodeRabbit started", "not provider start or the last observation"} {
						if !strings.Contains(co.Diagnostic.Explanation, phrase) {
							t.Fatal("trigger confirmation overstated provider progress", co)
						}
					}
					for _, phrase := range []string{"existing observer", "bounded status wait", "without reposting", "no provider-start time", "exact next-observation deadline", "stage and evidence_id", "do not request another review"} {
						if !strings.Contains(co.Diagnostic.NextAction, phrase) {
							t.Fatal("missing safe next action or unknown timing", co)
						}
					}
					b, err := json.Marshal(co)
					if err != nil || len(b) > 2048 {
						t.Fatal("unbounded trigger diagnostic", err)
					}
				}
			}
		}
	}
}

func TestAgentLateHostedAllowanceRetryIsOnlyKnownTerminalPreparation(t *testing.T) {
	base := ReviewOperationStatus{ID: "original", Channel: "hosted", State: "held", Reason: "hosted_allowance_reply_pending", Stage: "held", NextAction: "read_diagnostics", AttemptID: "original_attempt", Revision: 3}
	for _, change := range []func(*ReviewOperationStatus){nil, func(v *ReviewOperationStatus) { v.Channel = "cli" }, func(v *ReviewOperationStatus) { v.State = "uncertain" }, func(v *ReviewOperationStatus) { v.State = "active" }, func(v *ReviewOperationStatus) { v.Reason = "hosted_allowance_unproven" }, func(v *ReviewOperationStatus) { v.Reason = "trigger_post_unproven" }} {
		v := base
		if change != nil {
			change(&v)
		}
		want := v
		if change == nil {
			want.NextAction = "new_request_to_recheck_allowance"
		}
		out := AgentReviewOperationStatus(v)
		if out.ReviewOperationStatus != want {
			t.Fatal("late-query guidance changed facts or unrelated next action", out, want)
		}
		if change != nil {
			continue
		}
		if out.Diagnostic == nil || !strings.Contains(out.Diagnostic.Explanation, "hold is terminal") || !strings.Contains(out.Diagnostic.Explanation, "late reply does not resume") {
			t.Fatal("invented automatic continuation", out)
		}
		for _, phrase := range []string{"still-authorized planned review", "fresh idempotency key", "current configuration/head", "fresh allowance", "no manual GitHub repair", "known provider cooldown", "old available reply is not fresh permission", "unknown action outcome must keep its original key"} {
			if !strings.Contains(out.Diagnostic.NextAction, phrase) {
				t.Fatal("missing safe continuation distinction", out)
			}
		}
		b, err := json.Marshal(out)
		if err != nil || len(b) > 2048 {
			t.Fatal("unbounded late-query diagnostic", err)
		}
	}
}

func TestAgentCLIAllowanceTimeoutRecoveryPreservesOriginalHold(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, state := range []string{"held", "active", "uncertain", "completed"} {
			v := ReviewOperationStatus{ID: "original_operation", Channel: channel, State: state, Reason: "allowance_timeout", Stage: "held", NextAction: "read_diagnostics", AttemptID: "original_attempt", Revision: 3, UpdatedAt: "original_time"}
			full := ReviewOperation{ID: v.ID, Channel: v.Channel, State: v.State, Reason: v.Reason, AttemptID: v.AttemptID, Revision: v.Revision, UpdatedAt: v.UpdatedAt}
			got, detail := AgentReviewOperationStatus(v), AgentReviewOperation(full)
			want := v
			diagnosed := channel == "cli" && state == "held"
			if diagnosed {
				want.NextAction = "retry_later_with_new_request"
			}
			if got.ReviewOperationStatus != want || detail.ReviewOperation != full || !reflect.DeepEqual(got.Diagnostic, detail.Diagnostic) || (got.Diagnostic != nil) != diagnosed {
				t.Fatal("timeout advice altered scope/state/identity", got)
			}
			if diagnosed {
				for _, part := range []string{"allowance-check deadline", "No review command was started", "terminal", "neither exhausted quota nor a provider outage", "unknown"} {
					if !strings.Contains(got.Diagnostic.Explanation, part) {
						t.Fatal("timeout diagnostic lost known/unknown boundary", part)
					}
				}
				for _, part := range []string{"avoid rapid retries", "remains authorized", "fresh idempotency key", "current configuration/head", "original key replays", "Do not reset"} {
					if !strings.Contains(got.Diagnostic.NextAction, part) {
						t.Fatal("timeout recovery lost safeguard", part)
					}
				}
				body, _ := json.Marshal(got)
				if len(body) > 2048 {
					t.Fatal("timeout diagnostic too large")
				}
			}
		}
	}
}
