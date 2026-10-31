package client

import (
	"strings"
	"testing"
)

func TestHostedRateLimitedOperationExplainsTerminalNoCreditBoundary(t *testing.T) {
	v := ReviewOperationStatus{Channel: "hosted", State: "rate_limited", Reason: "hosted_review_rate_limited", Stage: "held", NextAction: "read_diagnostics"}
	got := AgentReviewOperationStatus(v)
	want := v
	want.NextAction = "retry_later_with_new_request"
	if got.ReviewOperationStatus != want || got.Diagnostic == nil || !strings.Contains(got.Diagnostic.Explanation, "terminal") || !strings.Contains(got.Diagnostic.Explanation, "no completed review") || !strings.Contains(got.Diagnostic.NextAction, "fresh request/key") || !strings.Contains(got.Diagnostic.NextAction, "no exact retry deadline") {
		t.Fatal("missing safe rejection guidance", got)
	}
	v.Channel = "cli"
	if AgentReviewOperationStatus(v).Diagnostic != nil {
		t.Fatal("hosted diagnostic applied to another provider path")
	}
}
