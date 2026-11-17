package client

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCIWatchInterruptionCopiesOnlyLocalErrors(t *testing.T) {
	for _, code := range []string{"canceled", "deadline_exceeded"} {
		original := &Error{Code: code, Message: "original", Action: "original action", Details: map[string]string{"safe": "retained"}, RequestID: "request-original", OutcomeUnknown: true}
		got := ciWatchReadError(fmt.Errorf("wrapped: %w", original)).(*Error)
		if got == original || got.Code != original.Code || got.Status != original.Status || got.RequestID != original.RequestID || got.OutcomeUnknown != original.OutcomeUnknown || !reflect.DeepEqual(got.Details, original.Details) || original.Message != "original" || original.Action != "original action" {
			t.Fatal("CI hint changed original error facts", got, original)
		}
	}
	for _, original := range []*Error{{Code: "canceled", Status: 503}, {Code: "deadline_exceeded", Status: 504}, {Code: "overloaded", Status: 503}, {Code: "connection_failed"}} {
		if ciWatchReadError(original) != original {
			t.Fatal("CI hint overwrote a nonlocal/cause-specific error", original)
		}
	}
}

func TestCIWatchResponseLimitOffersSmallerBatchWithoutChangingFacts(t *testing.T) {
	original := &Error{Code: "ci_watch_response_limit", Status: 503, Message: "safe refusal", Action: "old", RequestID: "request-limit", Details: map[string]any{"reason": "ci_watch_response_limit"}}
	var got *Error
	if !errors.As(ciWatchReadError(fmt.Errorf("wrapped: %w", original)), &got) {
		t.Fatal("missing structured error")
	}
	if got == original || got.Action == original.Action || !strings.Contains(got.Action, "fewer targets") || got.Code != original.Code || got.Status != original.Status || got.Message != original.Message || got.RequestID != original.RequestID || !reflect.DeepEqual(got.Details, original.Details) {
		t.Fatal("response limit lost safe recovery or original facts", got)
	}
	if original.Action != "old" {
		t.Fatal("changed original refusal")
	}
}
