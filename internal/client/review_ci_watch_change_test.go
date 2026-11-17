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

func ciWatchChangeSample() (client.CIWatchTarget, client.CIWatchResult) {
	pin := client.CIWatchTarget{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), AfterObservationKey: strings.Repeat("c", 64)}
	return pin, client.CIWatchResult{ProjectID: "project-one", State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{{
		PullNumber: pin.PullNumber, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, State: "failed", Reason: "ci_required_checks_failed", BaseRef: "feature", GateRef: "main", ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: pin.BaseSHA, ObservedAt: "2026-11-01T00:00:00Z", CICommitSHA: pin.HeadSHA, Selection: "head", Required: 2, Failed: 1, Pending: 1,
		Checks: []client.ReviewCIContext{{Context: "build", State: "failed", Reason: "ci_failed", CheckRun: &client.ReviewCICheckRun{ID: 81, Status: "completed", Conclusion: "failure"}}, {Context: "test", State: "pending", Reason: "ci_status_pending"}}, ObservationKey: pin.AfterObservationKey, WaitingForChange: true,
	}}}
}

func TestCIWatchChangeClientExactReadAndTruthfulFailureTimeout(t *testing.T) {
	pin, response := ciWatchChangeSample()
	var mu sync.Mutex
	posts := 0
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FC-CI-Watch", "change-v3")
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
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("X-FC-CI-Watch") != "change-v3" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+testToken || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, []client.CIWatchTarget{pin}) {
			t.Error("known-failure read lost exact binding/baseline or became a mutation")
		}
		write(w, http.StatusOK, response)
	}))
	defer service.Close()
	c := newClient(t, service)
	got, err := c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || !reflect.DeepEqual(got, response) {
		t.Fatal("known failure timeout changed retained CI facts", got, err)
	}
	out := client.AgentCIWatch(got)
	if out.AllTargetsPassed || out.State != "pending" || !out.TimedOut || out.Targets[0].State != "failed" || out.Targets[0].Failed != 1 || out.Targets[0].Pending != 1 || out.Targets[0].Phase != "waiting_for_change" || out.Targets[0].Diagnostic == nil || !strings.Contains(out.Targets[0].Diagnostic.Explanation, "failure") || !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, response.Targets[0]) {
		t.Fatal("agent projection masked failure or claimed batch success", out)
	}
	// A changed failure wakes normally; the baseline is not a failure suppressor.
	mu.Lock()
	response.State, response.TimedOut = "failed", false
	response.Targets[0].ObservationKey, response.Targets[0].WaitingForChange = strings.Repeat("d", 64), false
	mu.Unlock()
	got, err = c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || got.State != "failed" || got.Targets[0].WaitingForChange || client.AgentCIWatch(got).AllTargetsPassed {
		t.Fatal("changed failure did not wake as failed", got, err)
	}
	mu.Lock()
	response.State = "passed"
	item := &response.Targets[0]
	item.State, item.Reason, item.Failed, item.Pending, item.Satisfied = "passed", "", 0, 0, 2
	item.Checks = []client.ReviewCIContext{{Context: "build", State: "satisfied"}, {Context: "test", State: "satisfied"}}
	mu.Unlock()
	got, err = c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || !client.AgentCIWatch(got).AllTargetsPassed {
		t.Fatal("changed passing proof did not wake", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 3 {
		t.Fatal("read was automatically retried", posts)
	}
}

func TestCIWatchChangeClientRejectsForgedWaitingFacts(t *testing.T) {
	for name, corrupt := range map[string]func(*client.CIWatchTarget, *client.CIWatchResult){
		"unsupplied baseline": func(p *client.CIWatchTarget, _ *client.CIWatchResult) { p.AfterObservationKey = "" },
		"different key": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].ObservationKey = strings.Repeat("d", 64)
		},
		"missing key": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservationKey = "" },
		"uppercase key": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].ObservationKey = strings.Repeat("A", 64)
		},
		"short key":      func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservationKey = "abc" },
		"pending target": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].State = "pending" },
		"omitted waiting flag": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WaitingForChange = false
			v.State, v.TimedOut = "failed", false
		},
		"terminal outer": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.State, v.TimedOut = "failed", false },
		"false pass":     func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.State, v.TimedOut = "passed", false },
	} {
		t.Run(name, func(t *testing.T) {
			pin, response := ciWatchChangeSample()
			corrupt(&pin, &response)
			posts := 0
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts++
				write(w, 200, response)
			}))
			defer service.Close()
			_, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			checkError(t, err, "invalid_response")
			if posts != 1 {
				t.Fatal("forged read was retried", posts)
			}
		})
	}
}

func TestCIWatchChangeClientInvalidBaselineStaysLocal(t *testing.T) {
	var absent *client.Client
	for _, key := range []string{"short", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("c", 65), " " + strings.Repeat("c", 64)} {
		pin, _ := ciWatchChangeSample()
		pin.AfterObservationKey = key
		_, err := absent.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
		checkError(t, err, "validation_failed")
	}
}

func TestCIWatchChangeClientStrictOptionalMetadataAndLegacyAbsence(t *testing.T) {
	pin, response := ciWatchChangeSample()
	original, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for name, replacement := range map[string]string{
		"null waiting flag": `"waiting_for_change":null`,
		"waiting alias":     `"WaitingForChange":true`,
		"duplicate waiting": `"waiting_for_change":true,"waiting_for_change":true`,
		"private detail":    `"waiting_for_change":true,"private_body":"PRIVATE_SENTINEL"`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(string(original), `"waiting_for_change":true`, replacement, 1)
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(raw))
			}))
			defer service.Close()
			_, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			checkError(t, err, "invalid_response", "PRIVATE_SENTINEL")
		})
	}
	// The optional metadata is not required for ordinary legacy target reads.
	pin.AfterObservationKey = ""
	response.State, response.TimedOut = "failed", false
	response.Targets[0].ObservationKey, response.Targets[0].WaitingForChange = "", false
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FC-CI-Watch", "change-v3")
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 200, response)
	}))
	defer service.Close()
	got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || !reflect.DeepEqual(got, response) {
		t.Fatal("missing optional metadata invalidated ordinary failed read", got, err)
	}
}
