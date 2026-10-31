package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func pendingStatusContext() ReviewCIContext {
	return ReviewCIContext{Context: "validate / branch barrier", State: "pending", Reason: "ci_status_pending", CheckRun: &ReviewCICheckRun{ID: 71, Status: "completed", Conclusion: "success"}, CommitStatus: &ReviewCICommitStatus{ID: 91, State: "pending"}}
}

func TestAgentPendingCommitStatusExplainsSameContextWithoutChangingFacts(t *testing.T) {
	for _, kind := range []string{"completed_check", "running_check", "status_only", "success_status", "check_only", "failed_context"} {
		t.Run(kind, func(t *testing.T) {
			check := pendingStatusContext()
			want := true
			switch kind {
			case "running_check":
				check.CheckRun.Status = "in_progress"
				check.CheckRun.Conclusion = ""
			case "status_only":
				check.CheckRun = nil
			case "success_status":
				check.CommitStatus.State = "success"
				want = false
			case "check_only":
				check.CommitStatus = nil
				want = false
			case "failed_context":
				check.State = "failed"
				want = false
			}
			checks := []ReviewCIContext{check}
			assessment := ReviewSourceCIAssessment{State: "pending", Checks: &checks, Required: 1, Pending: 1}
			watch := CIWatchResult{State: "pending", Targets: []CIWatchTargetResult{{State: "pending", Reason: "ci_required_checks_pending", ObservedAt: "2026-11-01T00:00:00Z", Required: 1, Pending: 1, Checks: checks}}}
			before, _ := json.Marshal([]any{assessment, watch})
			a := AgentReviewCI(assessment)
			w := AgentCIWatch(watch)
			if (a.Diagnostic != nil) != want || w.Targets[0].Diagnostic == nil {
				t.Fatal("wrong pending-status applicability", a, w)
			}
			if want {
				if a.Diagnostic.Explanation != w.Targets[0].Diagnostic.Explanation || a.Diagnostic.NextAction != w.Targets[0].Diagnostic.NextAction {
					t.Fatal("watch and assessment duplicated/drifted guidance")
				}
				text := a.Diagnostic.Explanation + " " + a.Diagnostic.NextAction
				for _, required := range []string{`"validate / branch barrier"`, "pending, not a CI failure", "commit status 91", "do not rerun CI"} {
					if !strings.Contains(text, required) {
						t.Fatal("missing concrete pending source", required, text)
					}
				}
				if check.CheckRun != nil && !strings.Contains(text, "check run 71") {
					t.Fatal("lost actual paired check ID", text)
				}
				if (strings.Contains(text, "completed successfully")) != (kind == "completed_check") {
					t.Fatal("invented successful completion", text)
				}
				if strings.Contains(text, "workflow run 71") || strings.Contains(text, "workflow run 91") || len(text) > 2000 || w.AllTargetsPassed {
					t.Fatal("invented association/credit or unbounded guidance", text)
				}
			} else if strings.Contains(w.Targets[0].Diagnostic.Explanation, "commit status 91") {
				t.Fatal("generic state invented pending commit status")
			}
			after, _ := json.Marshal([]any{assessment, watch})
			if string(before) != string(after) || !reflect.DeepEqual(a.ReviewSourceCIAssessment, assessment) || !reflect.DeepEqual(w.Targets[0].CIWatchTargetResult, watch.Targets[0]) {
				t.Fatal("diagnostic changed actual states/IDs")
			}
		})
	}
}

func TestAgentPendingCommitStatusKeepsHigherPriorityReasonsAndOneBoundedExample(t *testing.T) {
	checks := []ReviewCIContext{pendingStatusContext()}
	for _, reason := range []string{"ci_watch_check_pending", "ci_watch_refresh_pending", "ci_merge_base_mismatch"} {
		out := AgentCIWatch(CIWatchResult{State: "pending", Targets: []CIWatchTargetResult{{State: "pending", Reason: reason, ObservedAt: "2026-11-01T00:00:00Z", Pending: 1, Checks: checks}}})
		if out.Targets[0].Diagnostic == nil || strings.Contains(out.Targets[0].Diagnostic.Explanation, "commit status 91") {
			t.Fatal("pending status replaced higher-priority source/refresh reason", out)
		}
	}
	for _, state := range []string{"failed", "satisfied", "held", "unproven"} {
		if AgentReviewCI(ReviewSourceCIAssessment{State: state, Checks: &checks}).Diagnostic != nil {
			t.Fatal("pending context replaced overall state", state)
		}
	}
	first := pendingStatusContext()
	first.Context = "other"
	first.CheckRun = nil
	first.CommitStatus = &ReviewCICommitStatus{ID: 92, State: "pending"}
	checks = append([]ReviewCIContext{first}, checks...)
	out := AgentReviewCI(ReviewSourceCIAssessment{State: "pending", Checks: &checks})
	if out.Diagnostic == nil || !strings.Contains(out.Diagnostic.Explanation, "One required context") || !strings.Contains(out.Diagnostic.Explanation, "commit status 91") || strings.Contains(out.Diagnostic.Explanation, "commit status 92") {
		t.Fatal("did not select one helpful actual paired example", out)
	}
	checks[1].Context = strings.Repeat(`"`, 255)
	quoted := AgentReviewCI(ReviewSourceCIAssessment{State: "pending", Checks: &checks}).Diagnostic
	if quoted == nil || !strings.Contains(quoted.Explanation, `\"`) || len(quoted.Explanation)+len(quoted.NextAction) > 2000 {
		t.Fatal("context text was not quoted and bounded", quoted)
	}
}
