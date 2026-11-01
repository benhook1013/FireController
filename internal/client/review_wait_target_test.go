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
