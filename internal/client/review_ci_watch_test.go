package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func ciWatchPins() []client.CIWatchTarget {
	return []client.CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}, {PullNumber: 4, HeadSHA: strings.Repeat("c", 40)}}
}

func ciWatchSample() client.CIWatchResult {
	pins := ciWatchPins()
	return client.CIWatchResult{ProjectID: "project-one", State: "passed", Targets: []client.CIWatchTargetResult{
		{PullNumber: 2, HeadSHA: pins[0].HeadSHA, BaseSHA: pins[0].BaseSHA, State: "pending", Reason: "source_unproven", Required: 1, Failed: 1, Checks: []client.ReviewCIContext{{Context: "build", State: "failed", Reason: "ci_failed"}}},
		{PullNumber: 4, HeadSHA: pins[1].HeadSHA, State: "passed", BaseRef: "stack-parent", GateRef: "main", ObservedHeadSHA: pins[1].HeadSHA, ObservedBaseSHA: strings.Repeat("b", 40), ObservedAt: "2026-11-01T00:00:00Z", CICommitSHA: pins[1].HeadSHA, Selection: "head", Required: 1, Satisfied: 1, Checks: []client.ReviewCIContext{{Context: "required-build", State: "satisfied", CheckRun: &client.ReviewCICheckRun{ID: 81, Status: "completed", Conclusion: "success"}}}},
	}}
}

func TestCIWatchClientBoundReadAndStrictProjection(t *testing.T) {
	response := ciWatchSample()
	var mu sync.Mutex
	lost := false
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FC-CI-Watch", "change-v3")
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		posts++
		wantPreference := "wait=900"
		if lost {
			wantPreference = "wait=0"
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != wantPreference || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("CI read lost route/binding/preference or gained a mutation key")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 {
			t.Error("unexpected watch fields")
		}
		var pins []client.CIWatchTarget
		if json.Unmarshal(body["targets"], &pins) != nil || len(pins) != 2 || !reflect.DeepEqual(pins, ciWatchPins()) {
			t.Error("source pins changed")
		}
		if lost {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		write(w, 200, response)
	}))
	defer s.Close()
	c := newClient(t, s)
	got, err := c.WatchCI(t.Context(), ciWatchPins(), 900)
	if err != nil || got.State != "passed" || got.Targets[1].GateRef != "main" || got.Targets[1].BaseRef != "stack-parent" || got.Targets[1].Checks[0].CheckRun.ID != 81 {
		t.Fatal("fresh result or actionable check lost", got, err)
	}
	for name, corrupt := range map[string]func(*client.CIWatchResult){
		"project":        func(v *client.CIWatchResult) { v.ProjectID = "foreign" },
		"pin":            func(v *client.CIWatchResult) { v.Targets[0].HeadSHA = strings.Repeat("d", 40) },
		"base pin":       func(v *client.CIWatchResult) { v.Targets[0].BaseSHA = "" },
		"order":          func(v *client.CIWatchResult) { v.Targets[0], v.Targets[1] = v.Targets[1], v.Targets[0] },
		"top state":      func(v *client.CIWatchResult) { v.State = "pending" },
		"timeout credit": func(v *client.CIWatchResult) { v.TimedOut = true },
		"counts":         func(v *client.CIWatchResult) { v.Targets[1].Satisfied = 0 },
		"mismatch without facts": func(v *client.CIWatchResult) {
			v.Targets[1].Reason = "ci_merge_base_mismatch"
		},
		"passed with failure reason": func(v *client.CIWatchResult) {
			v.Targets[1].Reason = "ci_required_checks_failed"
		},
		"missing checks":      func(v *client.CIWatchResult) { v.Targets[1].Checks = nil },
		"wrong observed head": func(v *client.CIWatchResult) { v.Targets[1].ObservedHeadSHA = strings.Repeat("d", 40) },
		"unknown state":       func(v *client.CIWatchResult) { v.Targets[1].State = "healthy" },
		"missing gate":        func(v *client.CIWatchResult) { v.Targets[1].GateRef = "" },
	} {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			response = ciWatchSample()
			corrupt(&response)
			mu.Unlock()
			_, err := c.WatchCI(t.Context(), ciWatchPins(), 900)
			checkError(t, err, "invalid_response")
		})
	}
	mu.Lock()
	lost = true
	mu.Unlock()
	// An immediate read has no reconnect window. Positive-wait reconnection
	// keeps its independent restart/deadline proof in review_wait_reconnect_test.go.
	_, err = c.WatchCI(t.Context(), ciWatchPins(), 0)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.OutcomeUnknown {
		t.Fatal("lost read response was treated as an uncertain mutation", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 15 {
		t.Fatal("read retried", posts)
	}
}

func TestCIWatchClientInvalidTargetsStayLocal(t *testing.T) {
	var absent *client.Client
	for _, targets := range [][]client.CIWatchTarget{nil, {}, {{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), AfterObservationKey: strings.Repeat("c", 64)}}, {{PullNumber: 2, HeadSHA: "short"}}, {ciWatchPins()[0], ciWatchPins()[0]}, {{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: "short"}}} {
		_, err := absent.WatchCI(context.Background(), targets, 0)
		checkError(t, err, "validation_failed")
	}
	for _, seconds := range []int{-1, 901} {
		_, err := absent.WatchCI(context.Background(), ciWatchPins(), seconds)
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "validation_failed" || failure.OutcomeUnknown {
			t.Fatal("invalid read became an uncertain mutation", err)
		}
	}
	max := make([]client.CIWatchTarget, client.CIWatchMaxTargets)
	for i := range max {
		max[i] = client.CIWatchTarget{PullNumber: int64(i + 1), HeadSHA: strings.Repeat("a", 40)}
	}
	if client.ValidateCIWatchTargets(max) != nil || client.ValidateCIWatchTargets(append(max, client.CIWatchTarget{PullNumber: 21, HeadSHA: strings.Repeat("a", 40)})) == nil {
		t.Fatal("target bound changed")
	}
}

func TestCIWatchFailedTransportRetainsCompleteOriginalPins(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"connection", 0, "connection_failed"},
		{"bad-gateway", 502, "service_unavailable"},
		{"unavailable", 503, "service_unavailable"},
		{"gateway-timeout", 504, "service_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				if test.status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("temporary upstream failure"))
			}))
			defer service.Close()
			pins := ciWatchPins()
			pins[0].AfterObservationKey = strings.Repeat("c", 64)
			pins[0].FailureWorkflowIDs = []int64{17, 19}
			c := newClient(t, service)
			defer c.Close()
			_, err := c.WatchCI(t.Context(), pins, 0)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != test.code || failure.Status != test.status || failure.OutcomeUnknown {
				t.Fatal("transport classification changed", err)
			}
			details, ok := failure.Details.(map[string]any)
			if !ok || !reflect.DeepEqual(details["targets"], pins) {
				t.Fatal("terminal transport failure lost original pins", failure)
			}
			if test.status == 0 && details["transport"] == nil {
				t.Fatal("pin enrichment discarded transport proof")
			}
			if posts.Load() != 1 {
				t.Fatal("immediate read retried", posts.Load())
			}
		})
	}
}

func TestCIWatchCapabilityRefusalPrecedesStrictSuccessDecode(t *testing.T) {
	for _, test := range []struct {
		name, ack, body, code string
		status                int
		duplicate             bool
	}{
		{"legacy-shape", "", `{"legacy_result":true}`, "unsupported_operation", 200, false},
		{"old-version", "change-v2", `{malformed`, "unsupported_operation", 200, false},
		{"ambiguous-ack", "change-v3", `{malformed`, "unsupported_operation", 200, true},
		{"acknowledged-malformed", "change-v3", `{malformed`, "invalid_response", 200, false},
		{"auth-refusal", "", `{"error":{"code":"unauthorized","message":"Access refused."},"request_id":"request-one"}`, "unauthorized", 401, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				posts.Add(1)
				if test.ack != "" {
					w.Header().Add("X-FC-CI-Watch", test.ack)
				}
				if test.duplicate {
					w.Header().Add("X-FC-CI-Watch", test.ack)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer service.Close()
			c := newClient(t, service)
			defer c.Close()
			_, err := c.WatchCI(t.Context(), ciWatchPins(), 0)
			checkError(t, err, test.code)
			if posts.Load() != 1 {
				t.Fatal("capability refusal retried", posts.Load())
			}
		})
	}
}
