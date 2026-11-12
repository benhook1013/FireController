package client_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchBaselinedTerminalResponseRequiresObservationKey(t *testing.T) {
	for _, state := range []string{"passed", "failed"} {
		for _, key := range []string{"", strings.Repeat("d", 64)} {
			t.Run(state+"/"+key, func(t *testing.T) {
				pin, response := ciWatchChangeSample()
				if state == "passed" {
					pin, response = ciWatchPassedSample()
				}
				response.State, response.TimedOut = state, false
				response.Targets[0].WaitingForChange = false
				response.Targets[0].ObservationKey = key
				var posts atomic.Int64
				service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/instance" {
						handshake(w)
						return
					}
					posts.Add(1)
					w.Header().Set("X-FC-CI-Watch", "change-v3")
					write(w, 200, response)
				}))
				defer service.Close()
				got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
				if key == "" {
					checkError(t, err, "invalid_response")
				} else if err != nil || !reflect.DeepEqual(got, response) {
					t.Fatal("changed terminal proof was rejected", got, err)
				}
				if posts.Load() != 1 {
					t.Fatal("response validation retried the watch")
				}
			})
		}
	}
}

func TestAgentCIWatchReturnedBaselinesEndSubscriptionAndPinObservedBase(t *testing.T) {
	for _, kind := range []string{"passed", "failed", "workflow", "head only", "historical"} {
		for _, timedOut := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(kind+map[bool]string{false: "/batch", true: "/timeout"}[timedOut]+map[bool]string{false: "/implicit", true: "/explicit"}[explicit], func(t *testing.T) {
					_, response := ciWatchChangeSample()
					if kind == "passed" {
						_, response = ciWatchPassedSample()
					}
					item := &response.Targets[0]
					if !explicit {
						item.BaseSHA = ""
					}
					if kind == "workflow" || kind == "head only" || kind == "historical" {
						item.State = "pending"
						item.WorkflowFailures = []client.CIWorkflowFailure{{WorkflowID: 10, RunID: 100, RunAttempt: 1, JobID: 1000, CheckRunID: 10000, Name: "failed job", Conclusion: "failure"}}
						if kind == "head only" {
							item.WorkflowAttentionBasis = "head_only_current_comparison_unproven"
						} else if kind == "historical" {
							item.WorkflowFailures[0].Basis = "historical_same_head_failure_supersession_unproven"
						}
					}
					response.TimedOut = timedOut
					if !timedOut {
						response.State = "closed"
						response.Targets = append(response.Targets, client.CIWatchTargetResult{PullNumber: 3, State: "closed"})
					}
					out := client.AgentCIWatch(response)
					diagnostic := out.Targets[0].Diagnostic
					if diagnostic == nil || out.Targets[0].Phase != "waiting_for_change" || !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, response.Targets[0]) || !reflect.DeepEqual(out.CIWatchResult, response) {
						t.Fatal("rearm guidance changed the returned facts", out)
					}
					for _, required := range []string{"another bounded CI wait", "response ended the subscription", "observation_key as after_observation_key", "observed_base_sha as base_sha"} {
						if !strings.Contains(diagnostic.NextAction, required) {
							t.Fatal("missing safe rearm instruction", required, diagnostic)
						}
					}
					if strings.Contains(diagnostic.Explanation+diagnostic.NextAction, "wait pending") || strings.Contains(diagnostic.Explanation, "is awaiting") {
						t.Fatal("returned response claims a surviving wait", diagnostic)
					}
				})
			}
		}
	}
}
