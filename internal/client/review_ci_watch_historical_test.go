package client_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchHistoricalBasisStrictDecodeAndTruthfulMixedOutput(t *testing.T) {
	for _, mode := range []string{"valid", "known baseline", "head only", "mixed", "unknown basis", "test merge", "foreign head", "missing attempt", "unrequested"} {
		t.Run(mode, func(t *testing.T) {
			pin, response := ciWorkflowFailureSample()
			row := &response.Targets[0].WorkflowFailures[0]
			row.Basis = "historical_same_head_failure_supersession_unproven"
			row.Name = "Build and Test (account-service / 1)"
			switch mode {
			case "known baseline":
				pin.AfterObservationKey = response.Targets[0].ObservationKey
				response.State = "pending"
				response.TimedOut = true
				response.Targets[0].WaitingForChange = true
			case "head only":
				v := &response.Targets[0]
				v.WorkflowAttentionBasis = "head_only_current_comparison_unproven"
				v.Reason, v.Selection, v.CICommitSHA = "ci_merge_base_mismatch", "", ""
				v.Required, v.Satisfied, v.Pending = 0, 0, 0
				v.Checks = []client.ReviewCIContext{}
				v.MergeBaseMismatch = &client.CIMergeBaseMismatch{PRBaseSHA: pin.BaseSHA, LiveBaseSHA: strings.Repeat("d", 40), MergeSHA: strings.Repeat("e", 40), MergeBaseParentSHA: pin.BaseSHA}
			case "mixed":
				pin.FailureWorkflowIDs = append(pin.FailureWorkflowIDs, 302)
				next := *row
				next.Basis = ""
				next.WorkflowID++
				next.JobID++
				next.CheckRunID++
				response.Targets[0].WorkflowFailures = append(response.Targets[0].WorkflowFailures, next)
			case "unknown basis":
				row.Basis = "latest_failure"
			case "test merge":
				response.Targets[0].Selection = "test_merge"
				response.Targets[0].CICommitSHA = strings.Repeat("d", 40)
			case "foreign head":
				response.Targets[0].ObservedHeadSHA = strings.Repeat("d", 40)
			case "missing attempt":
				row.RunAttempt = 0
			case "unrequested":
				row.WorkflowID++
			}
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				if r.Header.Get("X-FC-CI-Workflow-Details") != "3" {
					t.Error("historical details not negotiated")
				}
				write(w, 200, response)
			}))
			defer service.Close()
			c := newClient(t, service)
			got, err := c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			valid := mode == "valid" || mode == "known baseline" || mode == "head only" || mode == "mixed"
			if !valid {
				if err == nil {
					t.Fatal("unsafe historical wire accepted", mode, got)
				}
				return
			}
			if err != nil {
				t.Fatal("valid historical wire rejected", err)
			}
			out := client.AgentCIWatch(got)
			d := out.Targets[0].Diagnostic
			if d == nil || !strings.HasPrefix(d.Explanation, "Confirmed job failure on this pinned PR head.") || !strings.Contains(d.Explanation, "whether they reran or replaced those failed jobs") {
				t.Fatal("confirmed failure obscured by unproven replacement", out)
			}
			if out.AllTargetsPassed || d == nil || !strings.Contains(d.Explanation, "do not prove that the latest/current comparison still fails") || !strings.Contains(d.Explanation, "run base is unknown") || strings.Contains(d.Explanation, "explicitly skipped that job") {
				t.Fatal("historical attention misrepresented coverage", out)
			}
			if !reflect.DeepEqual(out.CIWatchResult, got) || !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, got.Targets[0]) || out.Targets[0].State != "pending" || out.Targets[0].ObservationKey != response.Targets[0].ObservationKey {
				t.Fatal("diagnostic changed original gate facts or observation baseline", out)
			}
			if !strings.Contains(d.NextAction, "observation_key as after_observation_key") || !strings.Contains(d.NextAction, "Do not rerun CI or change required gates") {
				t.Fatal("historical attention lost safe quiet rearm guidance", d)
			}
			if mode == "head only" && !strings.Contains(d.NextAction, "Reconcile the actual PR/base comparison") {
				t.Fatal("historical signal hid comparison repair", d)
			}
			if mode == "mixed" && !strings.Contains(d.Explanation, "Unlabelled rows") {
				t.Fatal("mixed proven rows lost distinction", d)
			}
			if mode == "known baseline" && out.Targets[0].Phase != "waiting_for_change" {
				t.Fatal("quiet historical baseline lost", out)
			}
		})
	}
}
