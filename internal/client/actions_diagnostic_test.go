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

func actionsClientSample() client.ActionsDiagnostic {
	return client.ActionsDiagnostic{ProjectID: "project-one", Repository: "owner/repo", RepositoryID: 81, ObservedAt: "2026-11-17T12:00:00Z", AgeHours: 24, Limit: 10, Scanned: 3, Aged: 2, Omitted: 1, Capacity: "unknown", Completeness: "non_atomic_status_buckets", Runs: []client.ActionsDiagnosticRun{{ID: 11, CheckSuiteID: 22, WorkflowID: 33, RunNumber: 44, RunAttempt: 2, HeadSHA: strings.Repeat("a", 40), Event: "pull_request", Status: "queued", CreatedAt: "2026-09-14T12:00:00Z", UpdatedAt: "2026-09-15T12:00:00Z"}}}
}

func TestActionsDiagnosticClientBoundPostDefaultsAndNoInvalidInputRequest(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/actions-liveness/observe" || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("lost read binding or gained mutation key")
		}
		var request client.ActionsDiagnosticOptions
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid request body")
		}
		if request != (client.ActionsDiagnosticOptions{AgeHours: 24, Limit: 10}) && request != (client.ActionsDiagnosticOptions{AgeHours: 720, Limit: 20}) {
			t.Error("unexpected selector", request)
		}
		response := actionsClientSample()
		response.AgeHours = request.AgeHours
		response.Limit = request.Limit
		write(w, 200, response)
	}))
	defer server.Close()
	c := newClient(t, server)
	for _, options := range []client.ActionsDiagnosticOptions{{}, {AgeHours: 720, Limit: 20}} {
		result, err := c.ActionsLiveness(t.Context(), options)
		if err != nil || result.RepositoryID != 81 || result.Runs[0].RunAttempt != 2 || result.Omitted != 1 || result.Capacity != "unknown" {
			t.Fatal(result, err)
		}
	}
	for _, options := range []client.ActionsDiagnosticOptions{{AgeHours: -1}, {AgeHours: 721}, {Limit: -1}, {Limit: 21}} {
		result, err := c.ActionsLiveness(t.Context(), options)
		if err == nil || !reflect.DeepEqual(result, client.ActionsDiagnostic{}) {
			t.Fatal("invalid selectors returned facts", result, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("invalid selectors reached transport", calls.Load())
	}
}

func TestActionsDiagnosticClientRefusesMalformedFacts(t *testing.T) {
	cases := map[string]func(*client.ActionsDiagnostic){
		"project":                   func(v *client.ActionsDiagnostic) { v.ProjectID = "other" },
		"repository":                func(v *client.ActionsDiagnostic) { v.Repository = "owner/repo/other" },
		"repository ID":             func(v *client.ActionsDiagnostic) { v.RepositoryID = 0 },
		"observation":               func(v *client.ActionsDiagnostic) { v.ObservedAt = "bad" },
		"age echo":                  func(v *client.ActionsDiagnostic) { v.AgeHours = 25 },
		"limit echo":                func(v *client.ActionsDiagnostic) { v.Limit = 11 },
		"scan bound":                func(v *client.ActionsDiagnostic) { v.Scanned = 1001 },
		"aged count":                func(v *client.ActionsDiagnostic) { v.Aged = 4 },
		"omission":                  func(v *client.ActionsDiagnostic) { v.Omitted = 0 },
		"capacity":                  func(v *client.ActionsDiagnostic) { v.Capacity = "available" },
		"completeness":              func(v *client.ActionsDiagnostic) { v.Completeness = "complete" },
		"null rows":                 func(v *client.ActionsDiagnostic) { v.Runs = nil },
		"run ID":                    func(v *client.ActionsDiagnostic) { v.Runs[0].ID = 0 },
		"attempt":                   func(v *client.ActionsDiagnostic) { v.Runs[0].RunAttempt = 0 },
		"suite":                     func(v *client.ActionsDiagnostic) { v.Runs[0].CheckSuiteID = 0 },
		"head":                      func(v *client.ActionsDiagnostic) { v.Runs[0].HeadSHA = "short" },
		"event":                     func(v *client.ActionsDiagnostic) { v.Runs[0].Event = "private\ntext" },
		"terminal":                  func(v *client.ActionsDiagnostic) { v.Runs[0].Status = "completed" },
		"fresh alleged aged row":    func(v *client.ActionsDiagnostic) { v.Runs[0].UpdatedAt = v.ObservedAt },
		"future update":             func(v *client.ActionsDiagnostic) { v.Runs[0].UpdatedAt = "2026-11-18T12:00:00Z" },
		"reversed time":             func(v *client.ActionsDiagnostic) { v.Runs[0].CreatedAt = "2026-11-16T12:00:00Z" },
		"duplicate run":             func(v *client.ActionsDiagnostic) { v.Runs = append(v.Runs, v.Runs[0]); v.Omitted = 0 },
		"duplicate workflow number": func(v *client.ActionsDiagnostic) { r := v.Runs[0]; r.ID++; v.Runs = append(v.Runs, r); v.Omitted = 0 },
		"unordered": func(v *client.ActionsDiagnostic) {
			r := v.Runs[0]
			r.ID++
			r.RunNumber++
			r.UpdatedAt = "2026-09-14T13:00:00Z"
			v.Runs = append(v.Runs, r)
			v.Omitted = 0
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			response := actionsClientSample()
			change(&response)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				write(w, 200, response)
			}))
			defer server.Close()
			got, err := newClient(t, server).ActionsLiveness(t.Context(), client.ActionsDiagnosticOptions{})
			checkError(t, err, "invalid_response")
			if !reflect.DeepEqual(got, client.ActionsDiagnostic{}) || calls.Load() != 1 {
				t.Fatal("partial facts or retry", got, calls.Load())
			}
		})
	}
	for name, raw := range map[string]string{"truncated": "{", "unknown": `{"extra":true}`, "duplicate": `{"project_id":"project-one","project_id":"other"}`, "oversized": strings.Repeat(" ", 8193) + "{}"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(raw))
			}))
			defer server.Close()
			got, err := newClient(t, server).ActionsLiveness(t.Context(), client.ActionsDiagnosticOptions{})
			if err == nil || !reflect.DeepEqual(got, client.ActionsDiagnostic{}) {
				t.Fatal("malformed body credited", got, err)
			}
		})
	}
}

func TestActionsDiagnosticClientDoesNotRetryProviderOrGatewayRefusal(t *testing.T) {
	for _, status := range []int{401, 403, 429, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				write(w, status, map[string]any{"error": map[string]string{"code": "actions_diagnostic_unavailable", "message": "sensitive upstream raw detail"}})
			}))
			defer server.Close()
			got, err := newClient(t, server).ActionsLiveness(t.Context(), client.ActionsDiagnosticOptions{})
			var failure *client.Error
			if !errors.As(err, &failure) || failure.OutcomeUnknown || calls.Load() != 1 || !reflect.DeepEqual(got, client.ActionsDiagnostic{}) || strings.Contains(failure.Error(), "sensitive upstream") {
				t.Fatal("refusal retried, leaked or became uncertain mutation", got, err, calls.Load())
			}
		})
	}
}
