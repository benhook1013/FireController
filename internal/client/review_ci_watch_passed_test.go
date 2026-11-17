package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func ciWatchPassedSample() (client.CIWatchTarget, client.CIWatchResult) {
	pin, response := ciWatchChangeSample()
	item := &response.Targets[0]
	item.State, item.Reason = "passed", ""
	item.Satisfied, item.Failed, item.Pending = item.Required, 0, 0
	item.Checks = []client.ReviewCIContext{
		{Context: "build", State: "satisfied", CheckRun: &client.ReviewCICheckRun{ID: 81, Status: "completed", Conclusion: "success"}},
		{Context: "test", State: "satisfied"},
	}
	return pin, response
}

func TestCIWatchPassedBaselineClientPreservesPassFactsAndWaits(t *testing.T) {
	pin, response := ciWatchPassedSample()
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FC-CI-Watch", "change-v3")
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts.Add(1)
		var body struct {
			Targets []client.CIWatchTarget `json:"targets"`
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.Header.Get("X-FC-CI-Watch") != "change-v3" || r.Header.Get("Idempotency-Key") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body.Targets, []client.CIWatchTarget{pin}) {
			t.Error("passed rearm lost capability, original pins or read-only transport")
		}
		write(w, 200, response)
	}))
	defer service.Close()
	got, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || !reflect.DeepEqual(got, response) {
		t.Fatal("truthful same-key passing timeout rejected", got, err)
	}
	out := client.AgentCIWatch(got)
	if out.State != "pending" || !out.TimedOut || !out.AllTargetsPassed || out.Targets[0].Phase != "waiting_for_change" || out.Targets[0].Diagnostic == nil || !strings.Contains(out.Targets[0].Diagnostic.Explanation, "passed") || !strings.Contains(out.Targets[0].Diagnostic.Explanation, "observed_at") || strings.Contains(out.Targets[0].Diagnostic.Explanation, "failure") || !strings.Contains(out.Targets[0].Diagnostic.NextAction, "Pending checks alone") || !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, response.Targets[0]) {
		t.Fatal("passing rearm diagnostic changed facts or promised continuous readiness", out)
	}
	if posts.Load() != 1 {
		t.Fatal("passed rearm automatically repeated request", posts.Load())
	}
}

func TestCIWatchPassedBaselineClientRejectsForgedWaitingFacts(t *testing.T) {
	for name, damage := range map[string]func(*client.CIWatchTarget, *client.CIWatchResult){
		"unsupplied baseline": func(p *client.CIWatchTarget, _ *client.CIWatchResult) { p.AfterObservationKey = "" },
		"different key": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].ObservationKey = strings.Repeat("d", 64)
		},
		"missing key":               func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservationKey = "" },
		"malformed key":             func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservationKey = "short" },
		"pending instead of passed": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].State = "pending" },
		"waiting flag omitted": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].WaitingForChange = false
			v.State = "passed"
			v.TimedOut = false
		},
		"false passing gate": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].Satisfied--
			v.Targets[0].Pending++
			v.Targets[0].Checks[0].State = "pending"
		},
		"source moved": func(_ *client.CIWatchTarget, v *client.CIWatchResult) {
			v.Targets[0].ObservedHeadSHA = strings.Repeat("d", 40)
		},
		"not observed":   func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.Targets[0].ObservedAt = "" },
		"terminal outer": func(_ *client.CIWatchTarget, v *client.CIWatchResult) { v.State = "passed"; v.TimedOut = false },
	} {
		t.Run(name, func(t *testing.T) {
			pin, response := ciWatchPassedSample()
			damage(&pin, &response)
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-FC-CI-Watch", "change-v3")
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				write(w, 200, response)
			}))
			defer service.Close()
			_, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
			checkError(t, err, "invalid_response")
			if posts.Load() != 1 {
				t.Fatal("forged response automatically retried", posts.Load())
			}
		})
	}
}

func TestCIWatchPassedBaselineOldServerRefusalDoesNotFallbackOrRetry(t *testing.T) {
	pin, _ := ciWatchPassedSample()
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts.Add(1)
		if r.Header.Get("X-FC-CI-Watch") != "change-v3" {
			t.Error("client silently downgraded capability")
		}
		write(w, 400, map[string]any{"error": map[string]any{"code": "validation_failed", "message": "Unsupported watch baseline."}, "request_id": "old-watch"})
	}))
	defer service.Close()
	_, err := newClient(t, service).WatchCI(t.Context(), []client.CIWatchTarget{pin}, 30)
	checkError(t, err, "validation_failed")
	var refusal *client.Error
	if !errors.As(err, &refusal) || refusal.Status != 400 || refusal.RequestID != "old-watch" || refusal.Message != "The service refused this scoped request." || !strings.Contains(refusal.Action, "target fields") || !strings.Contains(refusal.Action, "matching client and server CI watch capabilities") || !strings.Contains(refusal.Action, "older server can") {
		t.Fatal("API refusal lost identity or supplied misleading capability guidance", err)
	}
	if posts.Load() != 1 {
		t.Fatal("old server refusal retried or silently polled", posts.Load())
	}
}

func TestCIWatchPassedBaselineRequiresExplicitServerAcknowledgement(t *testing.T) {
	for name, headers := range map[string][]string{
		"old server ignores header":  nil,
		"older capability":           {"change-v2"},
		"duplicate acknowledgements": {"change-v3", "change-v3"},
		"unsupported capability":     {"change-v4"},
	} {
		t.Run(name, func(t *testing.T) {
			pin, response := ciWatchPassedSample()
			// An old handler can ignore an unknown request header and return an ordinary
			// valid terminal response without the optional observation metadata.
			response.State, response.TimedOut = "passed", false
			response.Targets[0].WaitingForChange = false
			response.Targets[0].ObservationKey = ""
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				if r.Header.Get("X-FC-CI-Watch") != "change-v3" {
					t.Error("client silently downgraded capability")
				}
				for _, header := range headers {
					w.Header().Add("X-FC-CI-Watch", header)
				}
				write(w, 200, response)
			}))
			defer service.Close()
			c := newClient(t, service)
			for _, baseline := range []string{pin.AfterObservationKey, ""} {
				pin.AfterObservationKey = baseline
				_, err := c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 30)
				checkError(t, err, "unsupported_operation")
				var failure *client.Error
				if !errors.As(err, &failure) {
					t.Fatal(err)
				}
				if !strings.Contains(failure.Action, "deploy the backend") || !strings.Contains(failure.Action, "No automatic retry or fallback") {
					t.Fatal("unsupported capability did not explain deployment order", failure)
				}
			}
			if posts.Load() != 2 {
				t.Fatal("missing acknowledgement triggered retries or polling", posts.Load())
			}
		})
	}
}
