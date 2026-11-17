package client

import (
	"reflect"
	"strings"
	"testing"
)

func TestHostedActivityDiagnosticsKeepRetainedFactsAndTerminalBoundary(t *testing.T) {
	reasons := []string{"provider_activity_rate_limited", "provider_activity_unauthorized", "provider_activity_not_found", "provider_activity_response_invalid", "provider_activity_unavailable", "provider_activity_ambiguous", "provider_activity_state_unknown", "provider_activity_observation_invalid", "provider_review_active", "provider_activity_unproven", "provider_activity_changed"}
	for _, reason := range reasons {
		for _, state := range []string{"held", "failed"} {
			t.Run(state+"/"+reason, func(t *testing.T) {
				original := ReviewOperation{ID: "original-operation", ProjectID: "original-project", Channel: "hosted", State: state, Reason: reason, Revision: 3, AttemptID: "original-attempt", HeadSHA: "original-head"}
				full := AgentReviewOperation(original)
				compact := AgentReviewOperationStatus(ReviewOperationStatus{ID: original.ID, Channel: "hosted", State: state, Reason: reason, Revision: 3, AttemptID: original.AttemptID, NextAction: "contact_controller_owner"})
				if !reflect.DeepEqual(full.ReviewOperation, original) || full.Diagnostic == nil || !reflect.DeepEqual(full.Diagnostic, compact.Diagnostic) || compact.NextAction != "inspect_provider_activity" || compact.ID != original.ID || compact.Revision != original.Revision || compact.Reason != original.Reason {
					t.Fatal("activity diagnostic changed retained facts or needs a maintainer", full, compact)
				}
				if !strings.Contains(full.Diagnostic.Explanation, "No review trigger was posted") || strings.Contains(full.Diagnostic.NextAction, "contact_controller_owner") {
					t.Fatal("missing action boundary", full.Diagnostic)
				}
				if reason == "provider_activity_unproven" || reason == "provider_activity_changed" {
					if !strings.Contains(full.Diagnostic.Explanation, "subcause was not retained") {
						t.Fatal("historical cause invented", full.Diagnostic)
					}
				}
			})
		}
	}
	for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain", "completed", "cancelled", "rate_limited"} {
		for _, reason := range reasons {
			if got := hostedActivityDiagnostic("hosted", state, reason); got != nil {
				t.Fatal("terminal recovery supplied for running or unknown action", state, reason, got)
			}
		}
	}
	for _, reason := range append(reasons, "provider_activity_private_text") {
		if got := hostedActivityDiagnostic("cli", "held", reason); got != nil {
			t.Fatal("hosted guidance leaked into another channel", reason)
		}
	}
	if got := hostedActivityDiagnostic("hosted", "held", "provider_activity_private_text"); got != nil {
		t.Fatal("unknown reason accepted", got)
	}
}
