package client

import (
	"reflect"
	"strings"
	"testing"
)

func TestSchedulingDiagnosticsPreserveRetainedFacts(t *testing.T) {
	for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed", "automatic_intent_retained"} {
		v := ReviewOperationStatus{ID: "original", State: "held", Stage: "held", Reason: reason, Channel: "hosted", Revision: 3, NextAction: "read_diagnostics"}
		out := AgentReviewOperationStatus(v)
		if out.Diagnostic == nil || !reflect.DeepEqual(out.ReviewOperationStatus, v) || selectionSourceDiagnostic(reason) == nil {
			t.Fatal("missing scheduling diagnostic or changed original facts", reason)
		}
		if agentReviewOperationDiagnostic("hosted", "active", reason) != nil {
			t.Fatal("issued work gained scheduling refusal", reason)
		}
	}
	if schedulingDiagnostic("unknown") != nil {
		t.Fatal("invented scheduling refusal")
	}
}

func TestSchedulingDiagnosticsDoNotMislabelCLIAsHosted(t *testing.T) {
	for _, reason := range []string{"automation_authorization_changed", "automatic_intent_retained"} {
		value := AgentReviewOperationStatus(ReviewOperationStatus{State: "held", Stage: "held", Reason: reason, Channel: "cli", Revision: 1})
		if value.Diagnostic == nil || strings.Contains(value.Diagnostic.Explanation, "Hosted") {
			t.Fatal("CLI refusal was attributed to the other channel", value)
		}
	}
}

func TestQueuedSchedulingDiagnosticsUseCurrentCause(t *testing.T) {
	const due = "2026-11-11T12:00:00Z"
	for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed", "automatic_intent_retained"} {
		for _, channel := range []string{"cli", "hosted"} {
			for _, tc := range []struct {
				name, cause, explanation, action string
				wait                             *ReviewOperationWait
			}{
				{name: "awaiting_scheduler", cause: "awaiting_scheduler"},
				{name: "queue_order", cause: "queue_order"},
				{name: "absent_cause"},
				{name: "unknown_cause", cause: "unknown"},
				{name: "cooldown", cause: "cooldown", wait: &ReviewOperationWait{RetryNotBefore: due}, explanation: "retry_not_before", action: "Keep this"},
				{name: "queue_order_with_timer", cause: "queue_order", wait: &ReviewOperationWait{RetryNotBefore: due}, explanation: "retry_not_before", action: "Keep this"},
				{name: "quota_with_timer", cause: "cli_quota_wait", wait: &ReviewOperationWait{RetryNotBefore: due}, explanation: "retry_not_before", action: "Keep this"},
				{name: "operation_pause", cause: "operation_paused", wait: &ReviewOperationWait{Paused: true}, explanation: "no retry deadline is recorded", action: "Resume/cancel only this undispatched operation"},
				{name: "operation_pause_with_timer", cause: "operation_paused", wait: &ReviewOperationWait{Paused: true, RetryNotBefore: due}, explanation: "paused before", action: "Resume"},
				{name: "current_pr_pause", cause: "pull_scheduling_paused", wait: &ReviewOperationWait{RetryNotBefore: due}, explanation: "New reviews are paused for this PR", action: "Resume the PR only when authorized"},
				{name: "current_authorization_hold", cause: "automation_authorization_changed", wait: &ReviewOperationWait{RetryNotBefore: due}, explanation: "Automatic scheduling for this channel is off", action: "An authorized project manager"},
				{name: "current_retained_intent", cause: "automatic_intent_retained", explanation: "An automatic review intent already exists", action: "Inspect existing scoped operations"},
				{name: "quota_without_timer", cause: "cli_quota_wait"},
			} {
				t.Run(channel+"/"+reason+"/"+tc.name, func(t *testing.T) {
					explanation, action := tc.explanation, tc.action
					if tc.name == "quota_without_timer" && channel == "cli" {
						explanation, action = "queued for shared CLI quota rechecks", "Keep this original request queued"
					}
					operation := ReviewOperation{ID: "original", State: "queued", Reason: reason, Channel: channel, Revision: 3, Wait: tc.wait, queuedCause: tc.cause}
					status := ReviewOperationStatus{ID: operation.ID, State: operation.State, Stage: "queued", Reason: reason, Channel: channel, Revision: operation.Revision, Wait: tc.wait, queuedCause: tc.cause, NextAction: "wait"}
					full, compact := AgentReviewOperation(operation), AgentReviewOperationStatus(status)
					if !reflect.DeepEqual(full.ReviewOperation, operation) || !reflect.DeepEqual(compact.ReviewOperationStatus, status) || !reflect.DeepEqual(full.Diagnostic, compact.Diagnostic) {
						t.Fatal("wrappers changed retained facts or disagree", full, compact)
					}
					if explanation == "" {
						if full.Diagnostic != nil {
							t.Fatal("historical reason invented a current scheduling hold", full.Diagnostic)
						}
						return
					}
					if full.Diagnostic == nil || !strings.Contains(full.Diagnostic.Explanation, explanation) || !strings.Contains(full.Diagnostic.NextAction, action) {
						t.Fatal("current cause or retained operation wait guidance was masked", explanation, action, full.Diagnostic)
					}
				})
			}
		}
	}
}

func TestHeldSchedulingDiagnosticsIgnoreQueuedCause(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed", "automatic_intent_retained"} {
			operation := ReviewOperation{State: "held", Channel: channel, Reason: reason, queuedCause: "cooldown"}
			status := ReviewOperationStatus{State: "held", Stage: "held", Channel: channel, Reason: reason, queuedCause: "cooldown", NextAction: "read_diagnostics"}
			full, compact := AgentReviewOperation(operation), AgentReviewOperationStatus(status)
			if !reflect.DeepEqual(full.Diagnostic, schedulingDiagnostic(reason)) || !reflect.DeepEqual(compact.Diagnostic, full.Diagnostic) || !reflect.DeepEqual(full.ReviewOperation, operation) || !reflect.DeepEqual(compact.ReviewOperationStatus, status) {
				t.Fatal("held scheduling guidance or retained facts changed", full, compact)
			}
		}
	}
}
