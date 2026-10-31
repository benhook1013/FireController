package client

import (
	"fmt"
	"reflect"
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
