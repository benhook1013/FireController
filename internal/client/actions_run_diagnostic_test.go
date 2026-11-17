package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func actionsRunSample() client.ActionsRunDiagnostic {
	return client.ActionsRunDiagnostic{ProjectID: "project-one", Repository: "owner/repo", RepositoryID: 20, ObservedAt: "2026-11-17T12:00:00Z", Run: client.ActionsRunDiagnosticRun{ID: 50, CheckSuiteID: 40, WorkflowID: 30, RunNumber: 7, RunAttempt: 2, HeadSHA: strings.Repeat("c", 40), Event: "push", Status: "in_progress", CreatedAt: "2026-11-15T12:00:00Z", UpdatedAt: "2026-11-17T11:00:00Z"}, Limit: 10, Total: 3, Pending: 1, Running: 1, Completed: 1, Omitted: 2, Capacity: "unknown", Completeness: "non_atomic_attempt_jobs", Jobs: []client.ActionsRunDiagnosticJob{{ID: 1, CheckRunID: 101, Name: "executing", Status: "in_progress", StartedAt: "2026-11-17T11:00:00Z"}}}
}
func TestActionsRunDiagnosticClientExactReadDefaultsAndInvalidInputs(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.URL.Path != "/api/v1/projects/project-one/reviews/actions-run/observe" || r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("unbound or mutating request")
		}
		var args client.ActionsRunDiagnosticOptions
		if json.NewDecoder(r.Body).Decode(&args) != nil || args.RunID != 50 || args.RunAttempt != 2 || args.Limit != 10 {
			t.Error("selector changed", args)
		}
		write(w, 200, actionsRunSample())
	}))
	defer server.Close()
	c := newClient(t, server)
	got, err := c.ActionsRun(t.Context(), client.ActionsRunDiagnosticOptions{RunID: 50, RunAttempt: 2})
	if err != nil || !reflect.DeepEqual(got, actionsRunSample()) || calls.Load() != 1 {
		t.Fatal(got, err, calls.Load())
	}
	for _, args := range []client.ActionsRunDiagnosticOptions{{}, {RunID: 50}, {RunAttempt: 2}, {RunID: -1, RunAttempt: 2}, {RunID: 50, RunAttempt: -1}, {RunID: 50, RunAttempt: 2, Limit: -1}, {RunID: 50, RunAttempt: 2, Limit: 21}} {
		if _, err := c.ActionsRun(t.Context(), args); err == nil {
			t.Fatal("invalid options accepted", args)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid options reached API")
	}
}
func TestActionsRunDiagnosticClientRefusesInvalidFacts(t *testing.T) {
	cases := map[string]func(*client.ActionsRunDiagnostic){
		"project": func(v *client.ActionsRunDiagnostic) { v.ProjectID = "foreign" }, "repo": func(v *client.ActionsRunDiagnostic) { v.Repository = "foreign/path/extra" }, "repo_id": func(v *client.ActionsRunDiagnostic) { v.RepositoryID = 0 }, "run": func(v *client.ActionsRunDiagnostic) { v.Run.ID++ }, "attempt": func(v *client.ActionsRunDiagnostic) { v.Run.RunAttempt++ }, "suite": func(v *client.ActionsRunDiagnostic) { v.Run.CheckSuiteID = 0 }, "workflow": func(v *client.ActionsRunDiagnostic) { v.Run.WorkflowID = 0 }, "number": func(v *client.ActionsRunDiagnostic) { v.Run.RunNumber = 0 }, "head": func(v *client.ActionsRunDiagnostic) { v.Run.HeadSHA = "wrong" }, "event": func(v *client.ActionsRunDiagnostic) { v.Run.Event = "bad\nevent" }, "run_state": func(v *client.ActionsRunDiagnostic) { v.Run.Status = "unknown" }, "run_conclusion": func(v *client.ActionsRunDiagnostic) { v.Run.Conclusion = "success" }, "observation": func(v *client.ActionsRunDiagnostic) { v.ObservedAt = "wrong" }, "future_run": func(v *client.ActionsRunDiagnostic) { v.Run.UpdatedAt = "2027-01-01T00:00:00Z" }, "counts": func(v *client.ActionsRunDiagnostic) { v.Total++ }, "omitted": func(v *client.ActionsRunDiagnostic) { v.Omitted-- }, "negative": func(v *client.ActionsRunDiagnostic) { v.Pending = -1 }, "over_capacity": func(v *client.ActionsRunDiagnostic) { v.Total = 801; v.Pending = 799; v.Omitted = 800 }, "limit": func(v *client.ActionsRunDiagnostic) { v.Limit++ }, "capacity": func(v *client.ActionsRunDiagnostic) { v.Capacity = "available" }, "marker": func(v *client.ActionsRunDiagnostic) { v.Completeness = "atomic" }, "nil_jobs": func(v *client.ActionsRunDiagnostic) { v.Jobs = nil }, "job_id": func(v *client.ActionsRunDiagnostic) { v.Jobs[0].ID = 0 }, "check_id": func(v *client.ActionsRunDiagnostic) { v.Jobs[0].CheckRunID = 0 }, "job_name": func(v *client.ActionsRunDiagnostic) { v.Jobs[0].Name = "bad\nname" }, "job_state": func(v *client.ActionsRunDiagnostic) { v.Jobs[0].Status = "unknown" }, "job_conclusion": func(v *client.ActionsRunDiagnostic) { v.Jobs[0].Conclusion = "success" }, "missing_start": func(v *client.ActionsRunDiagnostic) { v.Jobs[0].StartedAt = "" }, "bad_time": func(v *client.ActionsRunDiagnostic) { v.Jobs[0].StartedAt = "wrong" },
		"duplicate_job": func(v *client.ActionsRunDiagnostic) { v.Jobs = append(v.Jobs, v.Jobs[0]); v.Omitted-- }, "duplicate_check": func(v *client.ActionsRunDiagnostic) {
			j := v.Jobs[0]
			j.ID++
			v.Jobs = append(v.Jobs, j)
			v.Omitted--
			v.Running++
			v.Pending--
		}, "unordered": func(v *client.ActionsRunDiagnostic) {
			j := v.Jobs[0]
			j.ID = 2
			j.CheckRunID = 102
			v.Jobs = append([]client.ActionsRunDiagnosticJob{j}, v.Jobs...)
			v.Omitted--
			v.Running++
			v.Pending--
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			v := actionsRunSample()
			edit(&v)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				write(w, 200, v)
			}))
			defer server.Close()
			got, err := newClient(t, server).ActionsRun(t.Context(), client.ActionsRunDiagnosticOptions{RunID: 50, RunAttempt: 2})
			checkError(t, err, "invalid_response")
			if !reflect.DeepEqual(got, client.ActionsRunDiagnostic{}) || calls.Load() != 1 {
				t.Fatal("partial result/retry", got, calls.Load())
			}
		})
	}
}
func TestActionsRunDiagnosticClientPreservesCancelledSyntheticStartWithoutVerdict(t *testing.T) {
	v := actionsRunSample()
	v.Run.Status = "completed"
	v.Run.Conclusion = "cancelled"
	v.Pending, v.Running, v.Completed = 0, 0, 3
	v.Jobs[0].Status = "completed"
	v.Jobs[0].Conclusion = "cancelled"
	v.Jobs[0].StartedAt = "2026-11-17T13:00:00Z"
	v.Jobs[0].CompletedAt = "2026-11-17T11:00:00Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 200, v)
	}))
	defer server.Close()
	got, err := newClient(t, server).ActionsRun(t.Context(), client.ActionsRunDiagnosticOptions{RunID: 50, RunAttempt: 2})
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatal("canonical timestamps changed", got, err)
	}
}
func TestActionsRunDiagnosticClientNeverRetriesRefusalOrMalformedBody(t *testing.T) {
	for _, status := range []int{401, 403, 429, 502, 503, 504, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"broken":true}`))
			}))
			defer server.Close()
			got, err := newClient(t, server).ActionsRun(t.Context(), client.ActionsRunDiagnosticOptions{RunID: 50, RunAttempt: 2})
			if err == nil || calls.Load() != 1 || !reflect.DeepEqual(got, client.ActionsRunDiagnostic{}) {
				t.Fatal("bad result/retry", got, err, calls.Load())
			}
		})
	}
}
