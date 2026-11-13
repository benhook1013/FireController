package client

import (
	"strings"
	"testing"
)

func TestQueueMergedRefusalPreservesErrorContract(t *testing.T) {
	e := &Error{Code: "review_queue_merged", Message: "Original refusal", Action: "Original action", Status: 409, RequestID: "request_synthetic", Details: map[string]int64{"current_revision": 7}}
	if queueChangeError(e) != e || e.Code != "review_queue_merged" || e.Status != 409 || e.RequestID != "request_synthetic" || e.OutcomeUnknown || e.Details.(map[string]int64)["current_revision"] != 7 || !strings.Contains(e.Message, "Ordinary merged PRs") || !strings.Contains(e.Action, "reason=superseded") || !strings.Contains(e.Action, "preserves all history") {
		t.Fatal("merged guidance changed the refusal contract", e)
	}
	unknown := &Error{Code: "review_queue_merged", Message: "Preserve original request", Action: "Recover exact key", Status: 503, OutcomeUnknown: true}
	if queueChangeError(unknown) != unknown || unknown.Message != "Preserve original request" || unknown.Action != "Recover exact key" || !unknown.OutcomeUnknown {
		t.Fatal("guidance replaced unknown-write recovery", unknown)
	}
}
