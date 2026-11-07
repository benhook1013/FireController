package client

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReviewWaitInterruptionRetainsOriginalTargetWithoutChangingRefusal(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, cause := range []*Error{
		{Code: "overloaded", Status: 503, RequestID: "shutdown-request", Details: map[string]any{"reason": "result_wait_lifetime"}},
		contextError(context.Canceled), contextError(context.DeadlineExceeded),
	} {
		var failure *Error
		if !errors.As(reviewWaitTargetError(cause, id), &failure) || failure == cause || failure.Code != cause.Code || failure.Status != cause.Status || failure.RequestID != cause.RequestID || failure.Details.(map[string]any)["operation_id"] != id || failure.OutcomeUnknown {
			t.Fatal("targeted interruption changed refusal", failure)
		}
		if original, ok := cause.Details.(map[string]any); ok && original["operation_id"] != nil {
			t.Fatal("target changed original error")
		}
	}
	for _, cause := range []*Error{{Code: "forbidden", Status: 403}, {Code: "instance_mismatch", Status: 409}, {Code: "invalid_response"}, {Code: "overloaded", Status: 503, Details: map[string]any{"reason": "capacity"}}} {
		if reviewWaitTargetError(cause, id) != cause {
			t.Fatal("noninterruption refusal changed", cause)
		}
	}
}
