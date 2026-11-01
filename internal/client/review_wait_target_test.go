package client

import (
	"reflect"
	"strings"
	"testing"
)

func TestReviewWaitInterruptionKeepsImmutableOriginalIdentity(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	targets := []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40)}, {PullNumber: 7, HeadSHA: strings.Repeat("d", 40)}}
	for _, code := range []string{"canceled", "deadline_exceeded"} {
		original := &Error{Code: code, Message: "original message", Action: "original action", RequestID: "request-original", Details: map[string]any{"reason": "safe_local_reason"}}
		got := reviewWaitTargetError(original, id, nil).(*Error)
		if got == original || got.Code != code || got.Message != original.Message || got.Action != original.Action || got.RequestID != original.RequestID || got.Status != 0 || got.OutcomeUnknown || got.Details.(map[string]any)["operation_id"] != id || len(original.Details.(map[string]any)) != 1 {
			t.Fatal("original error/identity changed", got, original)
		}
		got = reviewWaitTargetError(original, "", targets).(*Error)
		pinned := got.Details.(map[string]any)["targets"].([]CIWatchTarget)
		if !reflect.DeepEqual(pinned, targets) {
			t.Fatal("pins changed", pinned)
		}
		saved := targets[0]
		targets[0].HeadSHA = strings.Repeat("e", 40)
		if !reflect.DeepEqual(pinned[0], saved) {
			t.Fatal("reattachment identity shared caller slice")
		}
		targets[0] = saved
	}
	for _, original := range []*Error{
		{Code: "overloaded", Status: 503},
		{Code: "overloaded", Status: 503, Details: map[string]any{"reason": "result_wait_lifetime_extra"}},
		{Code: "forbidden", Status: 403, Details: map[string]any{"reason": "result_wait_lifetime"}},
		{Code: "unauthorized", Status: 401},
		{Code: "canceled", Status: 503},
		{Code: "connection_failed"},
	} {
		if reviewWaitTargetError(original, id, nil) != original {
			t.Fatal("unproven interruption/authorization error overwritten", original)
		}
	}
}

func TestReviewWaitCIRefusalKeepsValidatedDeepClonedPinsOnly(t *testing.T) {
	for _, kind := range []struct {
		code   string
		status int
	}{{"overloaded", 503}, {"storage_unavailable", 500}} {
		for _, reason := range []string{"ci_watch_scope_read", "ci_watch_binding_read", "ci_watch_capacity", "ci_watch_observer_unavailable", "ci_watch_canceled"} {
			pins := []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), FailureWorkflowIDs: []int64{31, 32}}}
			original := &Error{Code: kind.code, Status: kind.status, Details: map[string]any{"reason": reason}}
			got := reviewWaitTargetError(original, "", pins).(*Error)
			if got == original || len(original.Details.(map[string]any)) != 1 {
				t.Fatal("CI annotation mutated original refusal", original)
			}
			saved := got.Details.(map[string]any)["targets"].([]CIWatchTarget)
			pins[0].HeadSHA = strings.Repeat("c", 40)
			pins[0].FailureWorkflowIDs[0] = 99
			if saved[0].HeadSHA != strings.Repeat("a", 40) || saved[0].FailureWorkflowIDs[0] != 31 {
				t.Fatal("CI refusal shared original pin slices", saved)
			}
		}
	}
	valid := []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}}
	for _, original := range []*Error{
		{Code: "forbidden", Status: 403, Details: map[string]any{"reason": "ci_watch_capacity"}},
		{Code: "unauthorized", Status: 401, Details: map[string]any{"reason": "ci_watch_binding_read"}},
		{Code: "overloaded", Status: 403, Details: map[string]any{"reason": "ci_watch_capacity"}},
		{Code: "overloaded", Status: 503, Details: map[string]any{"reason": "ci_watch_capacity_extra"}},
	} {
		if reviewWaitTargetError(original, "", valid) != original {
			t.Fatal("CI annotation changed unmatched authority/refusal", original)
		}
	}
	failure := &Error{Code: "overloaded", Status: 503, Details: map[string]any{"reason": "ci_watch_capacity"}}
	if reviewWaitTargetError(failure, "review_operation_"+strings.Repeat("a", 32), valid) != failure || reviewWaitTargetError(failure, "", nil) != failure || reviewWaitTargetError(failure, "", []CIWatchTarget{{PullNumber: 2, HeadSHA: "short"}}) != failure {
		t.Fatal("CI-only pin annotation escaped validated watch scope")
	}
}
