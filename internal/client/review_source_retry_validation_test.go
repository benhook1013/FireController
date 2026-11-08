package client

import (
	"strings"
	"testing"
)

func TestSourceRetryClientRejectsUnboundOrMalformedTiming(t *testing.T) {
	good := ReviewSourceRetry{AttemptID: "review_attempt_" + strings.Repeat("a", 32), ObservedAt: "2026-11-09T01:00:00Z", RetryNotBefore: "2026-11-09T01:01:00Z", Sequence: 1}
	if !validReviewOperationWait(&ReviewOperationWait{SourceRetry: &good}, "cli", "queued", "") {
		t.Fatal("valid source wait rejected")
	}
	for _, mutate := range []func(*ReviewSourceRetry){
		func(r *ReviewSourceRetry) { r.Sequence = 0 }, func(r *ReviewSourceRetry) { r.Sequence = 4 },
		func(r *ReviewSourceRetry) { r.AttemptID = "foreign" }, func(r *ReviewSourceRetry) { r.ObservedAt = "unknown" },
		func(r *ReviewSourceRetry) { r.RetryNotBefore = r.ObservedAt }, func(r *ReviewSourceRetry) { r.RetryNotBefore = "2026-11-11T00:00:00Z" },
	} {
		bad := good
		mutate(&bad)
		if validReviewOperationWait(&ReviewOperationWait{SourceRetry: &bad}, "cli", "queued", "") {
			t.Fatal("invalid source wait accepted", bad)
		}
	}
	for _, scope := range [][3]string{{"hosted", "queued", ""}, {"cli", "held", ""}, {"cli", "active", good.AttemptID}} {
		if validReviewOperationWait(&ReviewOperationWait{SourceRetry: &good}, scope[0], scope[1], scope[2]) {
			t.Fatal("source wait escaped queued CLI scope", scope)
		}
	}
}
