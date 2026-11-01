package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func ciWorkflowFailureSample() (client.CIWatchTarget, client.CIWatchResult) {
	pin := client.CIWatchTarget{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), FailureWorkflowIDs: []int64{301}}
	return pin, client.CIWatchResult{ProjectID: "project-one", State: "attention", Targets: []client.CIWatchTargetResult{{
		PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "pending", Reason: "ci_required_checks_pending", BaseRef: "feature", GateRef: "main", ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: pin.BaseSHA, ObservedAt: "2026-11-01T00:00:00Z", CICommitSHA: pin.HeadSHA, Selection: "head", Required: 2, Satisfied: 1, Pending: 1,
		Checks: []client.ReviewCIContext{{Context: "build", State: "satisfied"}, {Context: "test", State: "pending", Reason: "ci_status_pending"}}, ObservationKey: strings.Repeat("c", 64),
		WorkflowFailures: []client.CIWorkflowFailure{{WorkflowID: 301, RunID: 401, RunAttempt: 2, JobID: 501, CheckRunID: 601, Name: "unit tests", Conclusion: "failure"}},
	}}}
}

func TestCIWatchWorkflowFailuresClientAttentionAndKnownChangeWait(t *testing.T) {
	pin, response := ciWorkflowFailureSample()
	var mu sync.Mutex
	posts := 0
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		posts++
		var body struct {
			Targets []client.CIWatchTarget `json:"targets"`
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("X-FC-CI-Watch") != "change-v1" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, []client.CIWatchTarget{pin}) {
			t.Error("workflow attention changed exact read selection or binding")
		}
		write(w, 200, response)
	}))
	defer service.Close()
	c := newClient(t, service)
	got, err := c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || !reflect.DeepEqual(got, response) {
		t.Fatal("selected workflow failure was lost", got, err)
	}
	out := client.AgentCIWatch(got)
	if out.AllTargetsPassed || out.State != "attention" || out.Targets[0].State != "pending" || out.Targets[0].Failed != 0 || out.Targets[0].Pending != 1 || !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, response.Targets[0]) || out.Targets[0].Diagnostic == nil {
		t.Fatal("early attention changed the required gate or implied success", out)
	}
	mu.Lock()
	pin.AfterObservationKey = response.Targets[0].ObservationKey
	response.State, response.TimedOut, response.Targets[0].WaitingForChange = "pending", true, true
	mu.Unlock()
	got, err = c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || !reflect.DeepEqual(got, response) || client.AgentCIWatch(got).AllTargetsPassed {
		t.Fatal("same workflow failure could not wait while keeping true pending facts", got, err)
	}
	mu.Lock()
	response.State, response.TimedOut, response.Targets[0].WaitingForChange = "attention", false, false
	response.Targets[0].ObservationKey = strings.Repeat("d", 64)
	response.Targets[0].WorkflowFailures[0].RunAttempt++
	mu.Unlock()
	got, err = c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || got.State != "attention" || got.Targets[0].WorkflowFailures[0].RunAttempt != 3 {
		t.Fatal("changed workflow failure did not wake", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 3 {
		t.Fatal("workflow read automatically repeated", posts)
	}
}

func TestCIWatchWorkflowFailuresClientRejectsForgedRows(t *testing.T) {
	cases := map[string]func(*client.CIWatchTarget, *client.CIWatchResult){
		"unrequested workflow": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].WorkflowFailures[0].WorkflowID++ },
		"missing workflow id": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowFailures[0].WorkflowID = 0
		},
		"missing run id": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].WorkflowFailures[0].RunID = 0 },
		"missing attempt": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowFailures[0].RunAttempt = 0
		},
		"missing job id": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].WorkflowFailures[0].JobID = 0 },
		"missing check id": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowFailures[0].CheckRunID = 0
		},
		"negative run id": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].WorkflowFailures[0].RunID = -1 },
		"missing name":    func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].WorkflowFailures[0].Name = "" },
		"not a failure": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowFailures[0].Conclusion = "success"
		},
		"cancelled": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowFailures[0].Conclusion = "cancelled"
		},
		"missing conclusion": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WorkflowFailures[0].Conclusion = ""
		},
		"duplicate workflow": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			row := v.Targets[0].WorkflowFailures[0]
			row.JobID++
			row.CheckRunID++
			v.Targets[0].WorkflowFailures = append(v.Targets[0].WorkflowFailures, row)
		},
		"duplicate job": func(p *client.CIWatchTarget, v *client.CIWatchResult) {
			p.FailureWorkflowIDs = []int64{301, 302}
			row := v.Targets[0].WorkflowFailures[0]
			row.WorkflowID = 302
			row.CheckRunID++
			v.Targets[0].WorkflowFailures = append(v.Targets[0].WorkflowFailures, row)
		},
		"missing observation": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservedAt = "" },
		"foreign head": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].ObservedHeadSHA = strings.Repeat("d", 40)
		},
		"false waiting": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.State, v.TimedOut, v.Targets[0].WaitingForChange = "pending", true, true
		},
		"different baseline": func(p *client.CIWatchTarget, v *client.CIWatchResult) {
			p.AfterObservationKey = strings.Repeat("d", 64)
			v.State, v.TimedOut, v.Targets[0].WaitingForChange = "pending", true, true
		},
		"missing waiting flag": func(p *client.CIWatchTarget, v *client.CIWatchResult) {
			p.AfterObservationKey = v.Targets[0].ObservationKey
		},
		"false batch pass": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.State = "passed" },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			pin, response := ciWorkflowFailureSample()
			corrupt(&pin, &response)
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, response)
			}))
			defer service.Close()
			_, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			checkError(t, err, "invalid_response")
		})
	}
}

func TestCIWatchWorkflowFailuresClientConclusionsAndNormalTerminalStates(t *testing.T) {
	for _, conclusion := range []string{"failure", "timed_out", "action_required", "startup_failure"} {
		t.Run(conclusion, func(t *testing.T) {
			pin, response := ciWorkflowFailureSample()
			response.Targets[0].WorkflowFailures[0].Conclusion = conclusion
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, response)
			}))
			defer service.Close()
			got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			if err != nil || got.State != "attention" {
				t.Fatal("actual failure conclusion was refused", got, err)
			}
		})
	}
	for _, state := range []string{"passed", "closed", "superseded"} {
		t.Run(state, func(t *testing.T) {
			pin, response := ciWorkflowFailureSample()
			response.State, response.Targets[0].State = state, state
			response.Targets[0].WorkflowFailures = nil
			response.Targets[0].Reason = "ci_watch_pr_closed"
			if state == "superseded" {
				response.Targets[0].Reason = "ci_watch_source_changed"
				response.Targets[0].ObservedHeadSHA = strings.Repeat("d", 40)
			}
			if state == "passed" {
				response.Targets[0].Reason = "ci_satisfied"
				response.Targets[0].Satisfied, response.Targets[0].Pending = 2, 0
				response.Targets[0].Checks[1].State = "satisfied"
				response.Targets[0].Checks[1].Reason = ""
			}
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, response)
			}))
			defer service.Close()
			got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			if err != nil || got.State != state || client.AgentCIWatch(got).AllTargetsPassed != (state == "passed") {
				t.Fatal("workflow selection changed ordinary terminal semantics", got, err)
			}
		})
	}
}

func TestCIWatchWorkflowFailuresClientInvalidSelectionsStayLocal(t *testing.T) {
	var absent *client.Client
	tooMany := make([]int64, 17)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	for _, ids := range [][]int64{{}, {0}, {-1}, {301, 301}, tooMany} {
		pin, _ := ciWorkflowFailureSample()
		pin.FailureWorkflowIDs = ids
		_, err := absent.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
		checkError(t, err, "validation_failed")
	}
	pin, _ := ciWorkflowFailureSample()
	pin.FailureWorkflowIDs = make([]int64, 16)
	for i := range pin.FailureWorkflowIDs {
		pin.FailureWorkflowIDs[i] = int64(i + 1)
	}
	if err := client.ValidateCIWatchTargets([]client.CIWatchTarget{pin}); err != nil {
		t.Fatal("maximum bounded workflow selection refused", err)
	}
}
