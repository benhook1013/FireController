package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchHistoricalBasisStrictDecodeAndTruthfulMixedOutput(t *testing.T) {
	for _, mode := range []string{"valid", "known baseline", "mixed", "unknown basis", "test merge", "foreign head", "missing attempt", "unrequested"} {
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
			valid := mode == "valid" || mode == "known baseline" || mode == "mixed"
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
			if out.AllTargetsPassed || d == nil || !strings.Contains(d.Explanation, "do not prove that the latest/current comparison still fails") || !strings.Contains(d.Explanation, "run base is unknown") || strings.Contains(d.Explanation, "explicitly skipped that job") {
				t.Fatal("historical attention misrepresented coverage", out)
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
