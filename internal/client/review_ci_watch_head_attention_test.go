package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchHeadAttentionClientRejectsUnboundScopeOrCredit(t *testing.T) {
	for name, change := range map[string]func(*client.CIWatchTargetResult){
		"valid":            func(*client.CIWatchTargetResult) {},
		"missing basis":    func(v *client.CIWatchTargetResult) { v.WorkflowAttentionBasis = "" },
		"unknown basis":    func(v *client.CIWatchTargetResult) { v.WorkflowAttentionBasis = "current_required_failure" },
		"missing mismatch": func(v *client.CIWatchTargetResult) { v.MergeBaseMismatch = nil },
		"wrong reason":     func(v *client.CIWatchTargetResult) { v.Reason = "ci_merge_pending" },
		"foreign head":     func(v *client.CIWatchTargetResult) { v.ObservedHeadSHA = strings.Repeat("f", 40) },
		"foreign base":     func(v *client.CIWatchTargetResult) { v.MergeBaseMismatch.PRBaseSHA = strings.Repeat("f", 40) },
		"selected commit":  func(v *client.CIWatchTargetResult) { v.Selection = "head"; v.CICommitSHA = v.HeadSHA },
		"required credit": func(v *client.CIWatchTargetResult) {
			v.Required = 1
			v.Satisfied = 1
			v.Checks = []client.ReviewCIContext{{Context: "invented", State: "satisfied"}}
		},
		"unselected workflow": func(v *client.CIWatchTargetResult) { v.WorkflowFailures[0].WorkflowID++ },
		"no failures":         func(v *client.CIWatchTargetResult) { v.WorkflowFailures = nil },
	} {
		t.Run(name, func(t *testing.T) {
			pin, value := ciWorkflowFailureSample()
			v := &value.Targets[0]
			v.WorkflowAttentionBasis = "head_only_current_comparison_unproven"
			v.Reason, v.Selection, v.CICommitSHA = "ci_merge_base_mismatch", "", ""
			v.Required, v.Satisfied, v.Pending = 0, 0, 0
			v.Checks = []client.ReviewCIContext{}
			v.MergeBaseMismatch = &client.CIMergeBaseMismatch{PRBaseSHA: pin.BaseSHA, LiveBaseSHA: strings.Repeat("d", 40), MergeSHA: strings.Repeat("e", 40), MergeBaseParentSHA: pin.BaseSHA}
			change(v)
			posts := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts++
				if r.Header.Get("X-FC-CI-Workflow-Details") != "2" {
					t.Error("head-only shape not opted in")
				}
				write(w, 200, value)
			}))
			defer s.Close()
			got, err := newClient(t, s).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			if name != "valid" {
				checkError(t, err, "invalid_response")
				return
			}
			if err != nil || got.State != "attention" || posts != 1 {
				t.Fatal("valid head-only signal rejected or polled", got, err, posts)
			}
			out := client.AgentCIWatch(got)
			if out.AllTargetsPassed || out.Targets[0].Diagnostic == nil || !strings.Contains(out.Targets[0].Diagnostic.Explanation, "base/merge comparison is unknown") {
				t.Fatal("diagnostic invented applicable CI/base", out)
			}
		})
	}
}

func TestCIWatchInventoryIncompleteUsesCanonicalDiagnostic(t *testing.T) {
	for _, suffix := range []string{"", "_head_results", "_merge_results", "_results_recheck", "_merge_results_recheck"} {
		reason := "ci_provider_inventory_incomplete" + suffix
		canonical := client.AgentReviewCI(client.ReviewSourceCIAssessment{State: "unproven", Reason: reason}).Diagnostic
		out := client.AgentCIWatch(client.CIWatchResult{State: "pending", Targets: []client.CIWatchTargetResult{{State: "pending", Reason: reason}}})
		if canonical == nil || out.Targets[0].Diagnostic == nil || out.Targets[0].Diagnostic.Explanation != canonical.Explanation || out.Targets[0].Diagnostic.NextAction != canonical.NextAction {
			t.Fatal("wait lost known inventory-completeness diagnostic", reason, out)
		}
	}
}
