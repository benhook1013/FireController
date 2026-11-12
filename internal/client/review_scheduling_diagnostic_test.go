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

func TestSchedulingDiagnosticExplainsInterruptedAuthorization(t *testing.T) {
	diagnostic := schedulingDiagnostic("automation_authorization_changed")
	for _, want := range []string{"Re-enabling scheduling does not resume", "Inspect this retained operation", "new authorized request", "original identity"} {
		if diagnostic == nil || !strings.Contains(diagnostic.NextAction, want) {
			t.Fatalf("missing continuity guidance %q: %+v", want, diagnostic)
		}
	}
	if strings.Contains(diagnostic.Explanation, "Automatic Hosted") {
		t.Fatal("shared authorization diagnostic excluded CLI")
	}
}
