package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAgentSelectionSourceHoldIsBoundedAndNeverReplacesChild(t *testing.T) {
	reasons := []string{"pull_draft", "source_budget_exhausted", "source_fetch_timeout", "source_rate_limited", "source_unauthorized", "source_not_found", "source_response_invalid", "source_provider_unavailable", "source_unavailable", "PRIVATE_PROVIDER_DETAIL"}
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
				phrases := []string{"terminal and has no child", "After addressing", "if the review remains authorized", "new key and current configuration", "old key replays", "pending selections and selected child", "No round reset or extra grant"}
				if reason == "source_budget_exhausted" || reason == "source_fetch_timeout" || reason == "source_provider_unavailable" {
					phrases = []string{"An explicit held selection is terminal", "if the review remains authorized", "new key and current configuration", "old key replays", "eligible automatic selection may recover on its original identity", "after at least thirty seconds", "retained retry budget", "unchanged authorization", "Preserve the original selection", "does not establish eligibility or promise an exact retry time", "No round reset or extra grant"}
				}
				if reason == "pull_draft" {
					phrases = []string{"After marking the PR ready for review", "if this review remains authorized", "new key and current configuration/head", "old key replays this terminal hold", "Do not reset rounds, grant extra reviews or retry automatically", "normal admission and provider checks"}
					for _, phrase := range []string{"GitHub Draft", "initial review readiness", "does not erase earned review progress"} {
						if !strings.Contains(got.Diagnostic.Explanation, phrase) {
							t.Fatal("draft guidance lost retained progress", phrase)
						}
					}
				}
				for _, phrase := range phrases {
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
	for _, reason := range []string{"pull_draft", "source_budget_exhausted"} {
		for _, raw := range []ReviewRunState{{Selection: ReviewRunSelection{State: "held", Reason: reason, OperationID: "retained_child"}}, {Selection: ReviewRunSelection{State: "held", Reason: reason}, Operation: &ReviewOperation{ID: "retained_child"}}} {
			if AgentReviewSelection(raw).Diagnostic != nil {
				t.Fatal("half-linked selection got replacement guidance")
			}
		}
	}
}

func TestPausedReviewGuidanceRequiresRecordedDeadline(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		absent := operationWaitDiagnostic(&ReviewOperationWait{Paused: true}, channel)
		if !strings.Contains(absent.Explanation, "no retry deadline is recorded") || strings.Contains(absent.Explanation, "retry deadline and review intent are retained") {
			t.Fatal(channel, absent)
		}
		present := operationWaitDiagnostic(&ReviewOperationWait{Paused: true, RetryNotBefore: "2026-11-09T09:00:00Z"}, channel)
		if strings.Contains(present.Explanation, "no retry deadline is recorded") {
			t.Fatal(channel, present)
		}
	}
}
