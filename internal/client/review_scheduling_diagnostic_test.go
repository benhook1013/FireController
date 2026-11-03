package client

import (
	"reflect"
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
