package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func ciWorkflowObservationSample() (client.CIWatchTarget, client.CIWatchResult) {
	pin, value := ciWorkflowFailureSample()
	value.State, value.TimedOut = "pending", true
	value.Targets[0].WorkflowFailures = nil
	value.Targets[0].WorkflowObservations = []client.CIWorkflowObservation{{WorkflowID: 301, Basis: "latest_run_only", RunID: 401, RunAttempt: 2, Status: "completed", Conclusion: "success"}}
	return pin, value
}

func TestCIWatchWorkflowObservationsClientWireAndProofLimit(t *testing.T) {
	for _, mode := range []string{"details", "legacy server", "ordinary", "no current run"} {
		t.Run(mode, func(t *testing.T) {
			pin, value := ciWorkflowObservationSample()
			if mode == "legacy server" || mode == "ordinary" {
				value.Targets[0].WorkflowObservations = nil
			}
			if mode == "ordinary" {
				pin.FailureWorkflowIDs = nil
			}
			if mode == "no current run" {
				value.Targets[0].WorkflowObservations[0] = client.CIWorkflowObservation{WorkflowID: 301, Basis: "latest_run_only"}
			}
			posts := 0
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts++
				var body struct {
					Targets []client.CIWatchTarget `json:"targets"`
				}
				wantHeader := "2"
				if mode == "ordinary" {
					wantHeader = ""
				}
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("X-FC-CI-Watch") != "change-v2" || r.Header.Get("X-FC-CI-Workflow-Details") != wantHeader || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, []client.CIWatchTarget{pin}) {
					t.Error("workflow visibility changed read identity, selection, or negotiation")
				}
				write(w, 200, value)
			}))
			defer service.Close()
			got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			if err != nil || !reflect.DeepEqual(got, value) || posts != 1 {
				t.Fatal(got, err, posts)
			}
			out := client.AgentCIWatch(got)
			if out.AllTargetsPassed || out.State != "pending" || out.Targets[0].Pending != 1 || !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, value.Targets[0]) {
				t.Fatal("display upgraded gate or changed source facts", out)
			}
			explanation := out.Targets[0].Diagnostic.Explanation
			if strings.Contains(explanation, "latest run/current attempt") != (mode == "details" || mode == "no current run") {
				t.Fatal("proof limit not scoped to known selected observations", explanation)
			}
		})
	}
}

func TestCIWatchWorkflowObservationsClientRejectsUnboundFacts(t *testing.T) {
	cases := map[string]func(*client.CIWatchTarget, *client.CIWatchResult){
		"unrequested": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].WorkflowID++
		},
		"duplicate": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations = append(v.Targets[0].WorkflowObservations, v.Targets[0].WorkflowObservations[0])
		},
		"missing basis": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].Basis = ""
		},
		"invented basis": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].Basis = "all_jobs_checked"
		},
		"missing run": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].WorkflowObservations[0].RunID = 0 },
		"missing attempt": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].RunAttempt = 0
		},
		"negative run": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].RunID = -1
		},
		"missing status": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].Status = ""
		},
		"unsafe status": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].Status = "raw\nbody"
		},
		"large conclusion": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowObservations[0].Conclusion = strings.Repeat("a", 65)
		},
		"foreign head": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].ObservedHeadSHA = strings.Repeat("e", 40)
		},
		"foreign base": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].ObservedBaseSHA = strings.Repeat("e", 40)
		},
		"missing observation": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservedAt = "" },
		"missing key":         func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservationKey = "" },
		"refreshing": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].Reason = "ci_watch_refresh_pending"
		},
		"unrequested details": func(p *client.CIWatchTarget, _ *client.CIWatchResult) { p.FailureWorkflowIDs = nil },
		"conflicting failure": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			_, f := ciWorkflowFailureSample()
			v.State = "attention"
			v.TimedOut = false
			v.Targets[0].WorkflowFailures = f.Targets[0].WorkflowFailures
			v.Targets[0].WorkflowObservations[0].RunAttempt++
		},
		"terminal": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.State = "closed"
			v.TimedOut = false
			v.Targets[0].State = "closed"
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			pin, value := ciWorkflowObservationSample()
			damage(&pin, &value)
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, value)
			}))
			defer service.Close()
			_, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			checkError(t, err, "invalid_response")
		})
	}
}

func TestCIWatchWorkflowObservationsClientStrictNestedJSON(t *testing.T) {
	pin, value := ciWorkflowObservationSample()
	encoded, _ := json.Marshal(value)
	row := `{"workflow_id":301,"basis":"latest_run_only","run_id":401,"run_attempt":2,"status":"completed","conclusion":"success"}`
	for name, replacement := range map[string]string{
		"empty array": `[]`, "null array": `null`, "null row": `[null]`, "null field": `[{"workflow_id":301,"basis":null}]`,
		"private field":   `[{"workflow_id":301,"basis":"latest_run_only","raw_body":"private"}]`,
		"alias":           `[{"Workflow_id":301,"basis":"latest_run_only"}]`,
		"duplicate field": `[{"workflow_id":301,"workflow_id":301,"basis":"latest_run_only"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(string(encoded), "["+row+"]", replacement, 1)
			if raw == string(encoded) {
				t.Fatal("fixture replacement missed")
			}
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(raw))
			}))
			defer service.Close()
			_, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			checkError(t, err, "invalid_response")
		})
	}
}

func TestCIWatchWorkflowObservationsUsesSharedProviderDiagnostic(t *testing.T) {
	for _, reason := range []string{"ci_provider_invalid_response_check_run_time_order", "ci_provider_invalid_response_head_results_check_run_time_order", "ci_provider_invalid_response_merge_results_check_run_time_order", "ci_provider_invalid_response_results_recheck_check_run_time_order", "ci_provider_invalid_response_merge_results_recheck_check_run_time_order"} {
		value := client.CIWatchResult{State: "pending", Targets: []client.CIWatchTargetResult{{State: "pending", Reason: reason}}}
		got := client.AgentCIWatch(value)
		shared := client.AgentReviewCI(client.ReviewSourceCIAssessment{State: "unproven", Reason: reason}).Diagnostic
		if shared == nil || got.Targets[0].Diagnostic == nil || got.Targets[0].Diagnostic.Explanation != shared.Explanation || got.Targets[0].Diagnostic.NextAction != shared.NextAction || !reflect.DeepEqual(got.Targets[0].CIWatchTargetResult, value.Targets[0]) || got.AllTargetsPassed {
			t.Fatal("CI wait lost shared safe cause or changed gate facts", got)
		}
	}
}
