package client

import (
	"encoding/json"
	"fmt"
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

func TestAgentHostedAttributionWindowDiagnosticsPreserveOriginalPendingFacts(t *testing.T) {
	for _, state := range []string{"active", "uncertain", "posting", "queued", "held", "failed", "completed", "cancelled", "rate_limited"} {
		for _, channel := range []string{"hosted", "cli"} {
			for _, observerReason := range []string{"attribution_window_closed", "publication_not_proven", ""} {
				observer := &ReviewHostedObserverStatus{State: "waiting", Reason: observerReason, LastCheckAt: "original_check"}
				compact := ReviewOperationStatus{ID: "original_operation", AttemptID: "original_attempt", Channel: channel, State: state, Stage: "provider_wait", Reason: "trigger_recovered", NextAction: "wait", Revision: 7, UpdatedAt: "original_transition", Observer: observer}
				full := ReviewOperation{ID: compact.ID, AttemptID: compact.AttemptID, Channel: channel, State: state, Reason: compact.Reason, Revision: compact.Revision, UpdatedAt: compact.UpdatedAt, Observer: observer}
				co, fo := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
				want := channel == "hosted" && (state == "active" || state == "uncertain") && observerReason == "attribution_window_closed"
				original := compact
				if want {
					original.NextAction = "read_diagnostics"
				}
				if !reflect.DeepEqual(co.ReviewOperationStatus, original) || !reflect.DeepEqual(fo.ReviewOperation, full) || !reflect.DeepEqual(co.Diagnostic, fo.Diagnostic) {
					t.Fatal("diagnostic changed original facts or disagreed across compact/full", state, channel)
				}
				if !want {
					if !reflect.DeepEqual(co.Diagnostic, agentReviewOperationDiagnostic(channel, state, compact.Reason)) {
						t.Fatal("unrelated channel/state/reason gained attribution hint")
					}
					continue
				}
				for _, phrase := range []string{"prior observation", "original Hosted result", "edited or later comment", "does not prove the provider is still running"} {
					if !strings.Contains(co.Diagnostic.Explanation, phrase) {
						t.Fatal("missing attribution/progress distinction", co)
					}
				}
				for _, phrase := range []string{"original operation/request key", "existing trigger", "retained capture diagnostics", "continue other work", "Do not submit a new request, repost or reset"} {
					if !strings.Contains(co.Diagnostic.NextAction, phrase) {
						t.Fatal("missing supported safe next action", co)
					}
				}
				if strings.Contains(co.Diagnostic.NextAction, "maintainer") || strings.Contains(co.Diagnostic.NextAction, "contact_controller_owner") {
					t.Fatal("hint required escalation to product worker")
				}
			}
		}
	}
	for _, evidence := range []string{"", "original_evidence"} {
		v := ReviewOperationStatus{ID: "original", Channel: "hosted", State: "uncertain", Stage: "uncertain", Reason: "hosted_observation_pending", AttemptID: "original_attempt", EvidenceID: evidence, Observer: &ReviewHostedObserverStatus{State: "waiting", Reason: "attribution_window_closed"}, NextAction: "contact_controller_owner"}
		out := AgentReviewOperationStatus(v)
		want := "wait_for_observation"
		if evidence == "" {
			want = "read_diagnostics"
		}
		if out.NextAction != want {
			t.Fatal("captured evidence or uncertain pending action was misclassified", out)
		}
	}
}

func TestAgentHostedAttributionHintKeepsHigherPriorityDiagnostics(t *testing.T) {
	compact := ReviewOperationStatus{Channel: "hosted", State: "active", Reason: "trigger_recovered", AttemptID: "original_attempt", NextAction: "wait", Observer: &ReviewHostedObserverStatus{State: "waiting", Reason: "attribution_window_closed"}}
	full := ReviewOperation{Channel: compact.Channel, State: compact.State, Reason: compact.Reason, AttemptID: compact.AttemptID, Observer: compact.Observer}
	wait := &ReviewOperationWait{Paused: true}
	compact.Wait, full.Wait = wait, wait
	if !reflect.DeepEqual(AgentReviewOperationStatus(compact).Diagnostic, operationWaitDiagnostic(wait)) || !reflect.DeepEqual(AgentReviewOperation(full).Diagnostic, operationWaitDiagnostic(wait)) || AgentReviewOperationStatus(compact).NextAction != "wait" {
		t.Fatal("attribution hint replaced existing operation wait")
	}
	pending := &ReviewPendingEvidence{Items: []ReviewPendingEvidenceItem{}}
	compact.PendingEvidence, full.PendingEvidence = pending, pending
	if !reflect.DeepEqual(AgentReviewOperationStatus(compact).Diagnostic, pendingEvidenceDiagnostic(pending)) || !reflect.DeepEqual(AgentReviewOperation(full).Diagnostic, pendingEvidenceDiagnostic(pending)) || AgentReviewOperationStatus(compact).NextAction != "new_request_if_authorized" {
		t.Fatal("attribution hint replaced pending evidence")
	}
}

func TestAgentOperationParentDiagnosticsPreserveCompactAndFullFacts(t *testing.T) {
	for _, state := range []string{"held", "queued", "preparing", "active", "completed", "failed", "uncertain", "rate_limited", "cancelled"} {
		for _, reason := range []string{"parent_moved", "predecessor_parent_moved", "parent_unavailable", "other_reason"} {
			compact := ReviewOperationStatus{ID: "original_operation", PullNumber: 3300, Channel: "hosted", State: state, Revision: 5, Stage: "held", Reason: reason, AttemptID: "original_attempt", EvidenceID: "original_evidence", UpdatedAt: "original_time", NextAction: "read_diagnostics"}
			full := ReviewOperation{ID: compact.ID, ProjectID: "original_project", ActorID: "original_actor", WorkerID: "original_worker", ConfigurationRevision: 12, PolicyRevision: 2, CapabilityFingerprint: "original_binding", PullNumber: compact.PullNumber, Channel: compact.Channel, HeadSHA: "original_head", AllowOutOfOrder: true, State: state, Revision: compact.Revision, Reason: reason, AttemptID: compact.AttemptID, CreatedAt: "original_creation", UpdatedAt: compact.UpdatedAt}
			compactOut, fullOut := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
			wantHint := state == "held" && (reason == "parent_moved" || reason == "predecessor_parent_moved" || reason == "parent_unavailable")
			if !reflect.DeepEqual(compactOut.ReviewOperationStatus, compact) || !reflect.DeepEqual(fullOut.ReviewOperation, full) || !reflect.DeepEqual(compactOut.Diagnostic, fullOut.Diagnostic) || (compactOut.Diagnostic != nil) != wantHint {
				t.Fatal("parent diagnostic changed facts, disagreed across views or invented a hold", state, reason)
			}
			if wantHint {
				hint := compactOut.Diagnostic
				if reason == "parent_moved" && (!strings.Contains(hint.Explanation, "Older operations") || !strings.Contains(hint.Explanation, "does not identify")) || reason == "predecessor_parent_moved" && (!strings.Contains(hint.Explanation, "selected PR's source check passed") || !strings.Contains(hint.Explanation, "ancestor")) {
					t.Fatal("parent diagnosis invented an edge or lost the proved boundary", hint)
				}
				requiredPhrases := []string{"owning workflow", "stable selected comparison", "current review authorization", "do not automatically retry", "reset review progress", "no-op commit"}
				if reason == "parent_unavailable" {
					if !strings.Contains(hint.Explanation, "did not retain which comparison failed") || strings.Contains(hint.Explanation, "PR #") {
						t.Fatal("absent source metadata became an identified parent", hint)
					}
					requiredPhrases = []string{"owning workflow", "After the source is qualified", "still-authorized review", "No round reset or extra grant", "Out-of-order requests still check the target's own parent"}
				}
				for _, required := range requiredPhrases {
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

func TestAgentParentComparisonRecoveryUsesCheckedPRAndPreservesFacts(t *testing.T) {
	for _, reason := range []string{"parent_moved", "predecessor_parent_moved"} {
		t.Run(reason, func(t *testing.T) {
			checked := int64(3300)
			if reason == "predecessor_parent_moved" {
				checked = 3034
			}
			facts := &ReviewSourceRefusal{PullNumber: checked, ParentPullNumber: 3038, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}
			compact := ReviewOperationStatus{ID: "original_operation", PullNumber: 3300, State: "held", Reason: reason, Revision: 5, AttemptID: "original_attempt", SourceRefusal: facts, NextAction: "read_diagnostics"}
			full := ReviewOperation{ID: compact.ID, PullNumber: compact.PullNumber, State: compact.State, Reason: compact.Reason, Revision: compact.Revision, AttemptID: compact.AttemptID, SourceRefusal: facts}
			before, _ := json.Marshal([]any{compact, full})
			c, f := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
			if !reflect.DeepEqual(c.ReviewOperationStatus, compact) || !reflect.DeepEqual(f.ReviewOperation, full) || !reflect.DeepEqual(c.Diagnostic, f.Diagnostic) || c.Diagnostic == nil {
				t.Fatal("recovery guidance changed retained identity/facts or diverged across views")
			}
			hint := c.Diagnostic
			for _, phrase := range []string{fmt.Sprintf("Check current PR #%d,", checked), "direct parent PR #3038", "ancestry", "Align an actually mismatched link", "If the named base already matches the parent and the child includes it", "may be stale", "same existing base branch", "returned head is unchanged and base matches the parent", "afresh under current review authorization", "terminal held operation does not resume", "This diagnostic performs no mutation or retry"} {
				if !strings.Contains(hint.NextAction, phrase) {
					t.Fatal("recovery omitted verified-current-source conditions or terminal boundary", phrase, hint)
				}
			}
			if reason == "predecessor_parent_moved" && strings.Contains(hint.NextAction, "#3300") {
				t.Fatal("ancestor repair targeted outer selected PR")
			}
			checkedLabel, prefix := "selected PR", "Preparation checked "
			if reason == "predecessor_parent_moved" {
				checkedLabel, prefix = "ancestor PR", "The selected PR's source check passed; preparation checked "
			}
			wantExplanation := fmt.Sprintf("%s%s #%d against parent PR #%d: original observed base %s differs from direct-parent head %s recorded at the check. These are retained facts, not a current observation.", prefix, checkedLabel, checked, facts.ParentPullNumber, facts.ObservedBaseSHA, facts.ExpectedParentSHA)
			if hint.Explanation != wantExplanation || strings.Contains(hint.NextAction, "gh pr edit") || strings.Contains(hint.NextAction, "override") {
				t.Fatal("explanation changed or guidance invented a current base/override command", hint)
			}
			after, _ := json.Marshal([]any{c.ReviewOperationStatus, f.ReviewOperation})
			if string(before) != string(after) {
				t.Fatal("recovery changed retained receipt JSON")
			}
		})
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
		fullOut := AgentReviewOperation(full)
		if !reflect.DeepEqual(out.ReviewOperationStatus, want) || !reflect.DeepEqual(fullOut.ReviewOperation, full) || out.Diagnostic == nil || fullOut.Diagnostic == nil || !strings.Contains(strings.ToLower(out.Diagnostic.NextAction), "do not") || !strings.Contains(strings.ToLower(fullOut.Diagnostic.NextAction), "do not") {
			t.Fatal("missing safe guidance or changed original operation", out)
		}
		if reason == "hosted_allowance_reply_pending" && !reflect.DeepEqual(out.Diagnostic, fullOut.Diagnostic) {
			t.Fatal("pending-reply guidance disagreed across views")
		}
		if reason == "hosted_allowance_unproven" {
			if !strings.Contains(out.Diagnostic.NextAction, "diagnostics:true") || !strings.Contains(fullOut.Diagnostic.NextAction, "one later fresh request/key") || !strings.Contains(fullOut.Diagnostic.NextAction, "remains authorized") || !strings.Contains(fullOut.Diagnostic.NextAction, "rechecks current source, configuration, rounds and provider allowance") || strings.Contains(fullOut.Diagnostic.NextAction, "maintainer") {
				t.Fatal("full terminal diagnostic omitted bounded recovery or compact discovery", out, fullOut)
			}
		}
		for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain"} {
			v.State = state
			for _, query := range []*ReviewHostedQueryDiagnostic{nil, {QueryID: "original_query"}, {QueryID: "original_query", QueryURL: "https://github.com/owner/repo/pull/2#issuecomment-123"}} {
				full.State, full.HostedQuery = state, query
				compact, diagnostic := AgentReviewOperationStatus(v), AgentReviewOperation(full)
				if compact.Diagnostic != nil || diagnostic.Diagnostic != nil || !reflect.DeepEqual(compact.ReviewOperationStatus, v) || !reflect.DeepEqual(diagnostic.ReviewOperation, full) {
					t.Fatal("running or unknown outcome gained terminal fresh-request guidance or changed original facts", state, compact, diagnostic)
				}
			}
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
					for _, phrase := range []string{"existing observer", "bounded status wait", "without reposting", "observer.last_check_at", "observer.last_source_at", "observer.next_check_at", "not an exact deadline", "stage and evidence_id", "do not request another review"} {
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

func TestAgentMissingParentDiagnosticsNameOnlyOriginalRetainedFacts(t *testing.T) {
	for _, checked := range []int64{3300, 3298} {
		facts := &ReviewSourceRefusal{PullNumber: checked, BaseRef: "codex/former-parent"}
		compact := ReviewOperationStatus{ID: "original_operation", PullNumber: 3300, Channel: "cli", State: "held", Reason: "parent_unavailable", SourceRefusal: facts, NextAction: "read_diagnostics"}
		full := ReviewOperation{ID: compact.ID, PullNumber: compact.PullNumber, Channel: compact.Channel, State: compact.State, Reason: compact.Reason, SourceRefusal: facts}
		c, f := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
		if !reflect.DeepEqual(c.ReviewOperationStatus, compact) || !reflect.DeepEqual(f.ReviewOperation, full) || !reflect.DeepEqual(c.Diagnostic, f.Diagnostic) {
			t.Fatal("missing-parent hint changed retained facts or diverged", c, f)
		}
		if c.Diagnostic == nil || !strings.Contains(c.Diagnostic.Explanation, fmt.Sprintf("PR #%d", checked)) || !strings.Contains(c.Diagnostic.Explanation, facts.BaseRef) || !strings.Contains(c.Diagnostic.Explanation, "original lookup facts, not a current source observation") {
			t.Fatal("hint lost original bounded identity", c)
		}
		if facts.ParentPullNumber != 0 || facts.ObservedBaseSHA != "" || facts.ExpectedParentSHA != "" {
			t.Fatal("hint invented unavailable parent or SHA", facts)
		}
	}
}

func TestAgentCLIAllowanceUnprovenResponsePreservesOriginalHold(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, state := range []string{"held", "queued", "preparing", "active", "uncertain", "completed"} {
			status := ReviewOperationStatus{ID: "original_operation", Channel: channel, State: state, Reason: "allowance_response_unproven", Stage: "held", NextAction: "read_diagnostics", AttemptID: "original_attempt", Revision: 3, UpdatedAt: "original_time"}
			full := ReviewOperation{ID: status.ID, Channel: channel, State: state, Reason: status.Reason, AttemptID: status.AttemptID, Revision: status.Revision, UpdatedAt: status.UpdatedAt}
			compact, detail := AgentReviewOperationStatus(status), AgentReviewOperation(full)
			want := status
			diagnosed := channel == "cli" && state == "held"
			if diagnosed {
				want.NextAction = "retry_later_with_new_request"
			}
			if compact.ReviewOperationStatus != want || detail.ReviewOperation != full || !reflect.DeepEqual(compact.Diagnostic, detail.Diagnostic) || (compact.Diagnostic != nil) != diagnosed {
				t.Fatal("diagnostic changed retained identities/state or applied outside CLI preflight hold", compact, detail)
			}
			if !diagnosed {
				continue
			}
			for _, part := range []string{"completed successfully", "response could not be validated", "No review command started", "terminal hold", "does not establish", "remaining balance", "exhausted quota", "retry time"} {
				if !strings.Contains(compact.Diagnostic.Explanation, part) {
					t.Fatal("diagnostic confused unknown response with quota", part)
				}
			}
			for _, part := range []string{"remains authorized", "fresh idempotency key", "current configuration/head", "original key replays", "avoid rapid retries", "Do not reset"} {
				if !strings.Contains(compact.Diagnostic.NextAction, part) {
					t.Fatal("diagnostic omitted bounded recovery", part)
				}
			}
			if detail.Allowance != nil {
				t.Fatal("diagnostic invented an allowance observation")
			}
			body, err := json.Marshal(compact)
			if err != nil || len(body) > 2048 {
				t.Fatal("unbounded diagnostic", err)
			}
		}
	}
}

func TestAgentHostedTargetChangedKeepsOriginalFactsAndRequiresQualifiedNewIntent(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, state := range []string{"held", "queued", "active", "uncertain", "failed", "completed"} {
			compact := ReviewOperationStatus{ID: "original_operation", AttemptID: "original_attempt", Channel: channel, State: state, Stage: state, Reason: "target_changed", Revision: 5, UpdatedAt: "original_time", NextAction: "read_diagnostics"}
			full := ReviewOperation{ID: compact.ID, AttemptID: compact.AttemptID, Channel: channel, State: state, Reason: compact.Reason, HeadSHA: strings.Repeat("a", 40), Revision: compact.Revision, UpdatedAt: compact.UpdatedAt}
			got, fullGot := AgentReviewOperationStatus(compact), AgentReviewOperation(full)
			want := channel == "hosted" && state == "held"
			expected := compact
			if want {
				expected.NextAction = "qualify_current_target"
			}
			if !reflect.DeepEqual(got.ReviewOperationStatus, expected) || !reflect.DeepEqual(fullGot.ReviewOperation, full) || !reflect.DeepEqual(got.Diagnostic, fullGot.Diagnostic) || (got.Diagnostic != nil) != want {
				t.Fatal("target guidance altered retained comparison or escaped its hold", channel, state)
			}
			if !want {
				continue
			}
			for _, phrase := range []string{"requested open comparison", "terminal", "will not silently follow"} {
				if !strings.Contains(got.Diagnostic.Explanation, phrase) {
					t.Fatal("lost original comparison boundary", phrase)
				}
			}
			for _, phrase := range []string{"If the review remains authorized", "qualify the current open head", "known cooldown", "new key", "current configuration revision", "old key replays", "cannot reopen or rebind", "does not transfer the old timer", "queue order is unchanged"} {
				if !strings.Contains(got.Diagnostic.NextAction, phrase) {
					t.Fatal("missing supported recovery step", phrase)
				}
			}
			body, err := json.Marshal(got)
			if err != nil || len(body) > 2048 {
				t.Fatal("target diagnostic exceeded compact output", len(body), err)
			}
		}
	}
}

func TestAgentSelectionSourceHoldIsBoundedAndNeverReplacesChild(t *testing.T) {
	reasons := []string{"source_budget_exhausted", "source_fetch_timeout", "source_rate_limited", "source_unauthorized", "source_not_found", "source_response_invalid", "source_provider_unavailable", "source_unavailable", "PRIVATE_PROVIDER_DETAIL"}
	for _, reason := range reasons {
		for _, state := range []string{"held", "queued", "selecting", "selected", "empty"} {
			for _, child := range []bool{false, true} {
				raw := ReviewRunState{Selection: ReviewRunSelection{ID: "original_selection", ProjectID: "original_project", ActorID: "original_actor", WorkerID: "original_worker", Channel: "cli", State: state, Reason: reason, ConfigurationRevision: 7, PolicyRevision: 2, ExpectedPullNumber: 3365, Revision: 3, CreatedAt: "original_creation", UpdatedAt: "original_check"}}
				if child {
					raw.Selection.OperationID = "original_operation"
					raw.Operation = &ReviewOperation{ID: "original_operation", State: "active", Channel: "cli"}
				}
				got := AgentReviewSelection(raw)
				want := state == "held" && !child && reason != "PRIVATE_PROVIDER_DETAIL"
				if !reflect.DeepEqual(got.ReviewRunState, raw) || (got.Diagnostic != nil) != want {
					t.Fatal("selection explanation changed facts or suggested replacement", state, reason, child)
				}
				if !want {
					continue
				}
				for _, phrase := range []string{"terminal and has no child", "After addressing", "if the review remains authorized", "new key and current configuration", "old key replays", "pending selections and selected child", "No round reset or extra grant"} {
					if !strings.Contains(got.Diagnostic.NextAction, phrase) {
						t.Fatal("unsafe selection recovery", phrase)
					}
				}
				if reason == "source_unavailable" && !strings.Contains(got.Diagnostic.Explanation, "did not retain its exact failure cause") {
					t.Fatal("historical cause was invented")
				}
				body, err := json.Marshal(got)
				if err != nil || len(body) > 2048 || strings.Contains(string(body), "PRIVATE_PROVIDER_DETAIL") {
					t.Fatal("selection output overbudget or leaked source detail", err)
				}
				direct := AgentReviewRunResult(ReviewRunResult{Result: &raw})
				if !reflect.DeepEqual(direct, got) {
					t.Fatal("start response and retained read disagree")
				}
			}
		}
	}
	// Even malformed half-links must not produce a replacement hint.
	for _, raw := range []ReviewRunState{{Selection: ReviewRunSelection{State: "held", Reason: "source_budget_exhausted", OperationID: "retained_child"}}, {Selection: ReviewRunSelection{State: "held", Reason: "source_budget_exhausted"}, Operation: &ReviewOperation{ID: "retained_child"}}} {
		if AgentReviewSelection(raw).Diagnostic != nil {
			t.Fatal("half-linked selection got replacement guidance")
		}
	}
}
