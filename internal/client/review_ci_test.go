package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func sampleCIAssessment() client.ReviewSourceCIAssessment {
	source := sampleSourceObservation()
	return client.ReviewSourceCIAssessment{ProjectID: "project-one", RefreshID: sourceRefreshID, Source: "observed_ci", ConfigurationRevision: 1, SourceObservedAt: source.ObservedAt, EvaluatedAt: source.ObservedAt, Target: source, State: "satisfied", Selection: "head", SelectedCommitSHA: source.HeadSHA, MergeState: "conflicting", ObservedAt: source.ObservedAt, Required: 1, Satisfied: 1}
}

func TestReviewCILocalReadRejectsContradictoryOrCrossScopeFacts(t *testing.T) {
	var response atomic.Value
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/project-one/reviews/source/2/ci" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "" {
			t.Error("CI read changed route or requested remote refresh")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response.Load().(string)))
	}))
	defer service.Close()
	c := newClient(t, service)
	valid, _ := json.Marshal(sampleCIAssessment())
	response.Store(string(valid))
	if got, err := c.ReviewSourceCIAssessment(t.Context(), 2); err != nil || got.State != "satisfied" || calls.Load() != 1 {
		t.Fatal(got, err, calls.Load())
	}
	mutations := map[string]func(map[string]any){
		"wrong project":      func(v map[string]any) { v["project_id"] = "other" },
		"wrong pull":         func(v map[string]any) { v["target"].(map[string]any)["pull_number"] = 3 },
		"wrong commit":       func(v map[string]any) { v["selected_commit_sha"] = strings.Repeat("c", 40) },
		"wrong count":        func(v map[string]any) { v["required"] = 2 },
		"false pass":         func(v map[string]any) { v["satisfied"] = 0; v["pending"] = 1 },
		"unknown field":      func(v map[string]any) { v["raw_provider_capture"] = "private" },
		"missing count":      func(v map[string]any) { delete(v, "failed") },
		"null state":         func(v map[string]any) { v["state"] = nil },
		"future observation": func(v map[string]any) { v["observed_at"] = "2030-01-01T00:00:00Z" },
		"zero revision pass": func(v map[string]any) { v["configuration_revision"] = 0 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			_ = json.Unmarshal(valid, &value)
			mutate(value)
			body, _ := json.Marshal(value)
			response.Store(string(body))
			_, err := c.ReviewSourceCIAssessment(t.Context(), 2)
			checkError(t, err, "invalid_response")
		})
	}
}

func TestReviewCIRefreshPreservesOptionAndRejectsMismatchedOutcome(t *testing.T) {
	var include atomic.Bool
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		var request map[string]any
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&request) != nil || request["include_ci"] != true || r.Header.Get("Prefer") != "wait=2" {
			t.Error("CI option or selected wait lost")
		}
		w.Header().Set("Content-Type", "application/json")
		operation := sampleSourceOperation()
		operation.IncludeCI = include.Load()
		if operation.IncludeCI {
			summary := client.ReviewSourceCISummary{State: "satisfied", Selection: "head", SelectedCommitSHA: operation.HeadSHA, MergeState: "conflicting", ObservedAt: operation.UpdatedAt, Required: 1, Satisfied: 1}
			operation.CI = &summary
		}
		_ = json.NewEncoder(w).Encode(operation)
	}))
	defer service.Close()
	c := newClient(t, service)
	request := client.ReviewSourceRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), IncludeCI: true}
	include.Store(true)
	if got, err := c.RefreshReviewSource(t.Context(), "explicit-ci", request, 2); err != nil || got.Operation == nil || got.Operation.CI == nil {
		t.Fatal(got, err)
	}
	include.Store(false)
	_, err := c.RefreshReviewSource(t.Context(), "explicit-ci", request, 2)
	checkError(t, err, "invalid_response")
}
