package client_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestAgentCITimeOrderExplainsEndpointAndFieldsWithoutChangingFacts(t *testing.T) {
	for _, stage := range []string{"", "head_results_", "merge_results_", "results_recheck_", "merge_results_recheck_"} {
		value := sampleCIAssessment()
		value.State, value.Reason = "unproven", "ci_provider_invalid_response_"+stage+"check_run_time_order"
		value.Required, value.Satisfied = 0, 0
		out := client.AgentReviewCI(value)
		if !reflect.DeepEqual(out.ReviewSourceCIAssessment, value) || out.Diagnostic == nil {
			t.Fatal("diagnostic lost original unknown facts", out)
		}
		for _, fact := range []string{"check-runs endpoint", "completed_at earlier than started_at", "non-skipped", "unproven", "zero retained counts"} {
			if !strings.Contains(out.Diagnostic.Explanation, fact) {
				t.Fatal("missing finite provider fact", fact, out.Diagnostic)
			}
		}
		if !strings.Contains(out.Diagnostic.NextAction, "same comparison") || !strings.Contains(out.Diagnostic.NextAction, "Do not assume a clock offset") || strings.Contains(out.Diagnostic.NextAction, "controller owner") || strings.Contains(out.Diagnostic.NextAction, "report") {
			t.Fatal("diagnostic invented a bypass or escalation prerequisite", out.Diagnostic)
		}
	}
	unknown := sampleCIAssessment()
	unknown.State, unknown.Reason = "unproven", "ci_provider_invalid_response_head_results_PRIVATE"
	if out := client.AgentReviewCI(unknown); out.Diagnostic != nil {
		t.Fatal("unknown detail acquired a maintained diagnostic", out.Diagnostic)
	}
}

func TestAgentCIInvalidMetadataRecoveryDoesNotRequireOwnerEscalation(t *testing.T) {
	for _, reason := range []string{"ci_provider_invalid_response_head_results", "ci_provider_invalid_response_assessment"} {
		value := sampleCIAssessment()
		value.State, value.Reason, value.Required, value.Satisfied = "unproven", reason, 0, 0
		out := client.AgentReviewCI(value)
		if !reflect.DeepEqual(out.ReviewSourceCIAssessment, value) || out.Diagnostic == nil || strings.Contains(out.Diagnostic.NextAction, "report") || strings.Contains(out.Diagnostic.NextAction, "controller owner") || !strings.Contains(out.Diagnostic.NextAction, "historical result") || !strings.Contains(out.Diagnostic.NextAction, "later bounded CI wait") {
			t.Fatal("invalid metadata still requires an external debugging step", out)
		}
	}
}
