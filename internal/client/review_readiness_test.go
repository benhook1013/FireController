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

func sampleReadiness() client.ReviewSourceReadinessAssessment {
	source := sampleSourceObservation()
	return client.ReviewSourceReadinessAssessment{ProjectID: "project-one", RefreshID: sourceRefreshID, Source: "observed_comparison", ConfigurationRevision: 1, PolicyRevision: 1, SourceObservedAt: source.ObservedAt, EvaluatedAt: source.ObservedAt, Target: source, State: "ready_as_observed", Reasons: []string{}, Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}, CI: client.ReviewSourceCISummary{State: "not_required", MergeState: "verified", ObservedAt: source.ObservedAt}, ReviewState: client.ReviewSourceReviewStateSummary{State: "complete", ObservedAt: source.ObservedAt, PullState: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}}
}

func TestReadinessClientRejectsFalseGreenAndMalformedObservations(t *testing.T) {
	var response atomic.Value
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/source/2/readiness" || r.URL.RawQuery != "" || r.Header.Get("Prefer") != "" {
			t.Error("readiness read changed scope or refreshed source")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response.Load().(string)))
	}))
	defer service.Close()
	c := newClient(t, service)
	valid, _ := json.Marshal(sampleReadiness())
	response.Store(string(valid))
	if got, err := c.ReviewSourceReadiness(t.Context(), 2); err != nil || got.State != "ready_as_observed" || calls.Load() != 1 {
		t.Fatal(got, err, calls.Load())
	}
	mutations := map[string]func(map[string]any){
		"wrong project":      func(v map[string]any) { v["project_id"] = "other" },
		"wrong target":       func(v map[string]any) { v["target"].(map[string]any)["pull_number"] = 3 },
		"zero configuration": func(v map[string]any) { v["configuration_revision"] = 0 },
		"missing policy":     func(v map[string]any) { v["policy_revision"] = 0 },
		"empty channels":     func(v map[string]any) { v["channels"] = []any{} },
		"pending channel": func(v map[string]any) {
			v["channels"] = []any{map[string]any{"channel": "cli", "covered": true, "pending": true}}
		},
		"incoming pending":       func(v map[string]any) { v["incoming_routes_pending"] = 1 },
		"incoming capacity":      func(v map[string]any) { v["incoming_routes_capacity"] = true },
		"incoming negative":      func(v map[string]any) { v["incoming_routes_pending"] = -1 },
		"incoming null":          func(v map[string]any) { v["incoming_routes_pending"] = nil },
		"positive reason":        func(v map[string]any) { v["reasons"] = []string{"unknown_hold"} },
		"null reasons":           func(v map[string]any) { v["reasons"] = nil },
		"CI merge contradiction": func(v map[string]any) { v["ci"].(map[string]any)["merge_state"] = "conflicting" },
		"CI unproven": func(v map[string]any) {
			ci := v["ci"].(map[string]any)
			ci["state"] = "unproven"
			ci["reason"] = "ci_provider_unavailable"
		},
		"CI wrong head": func(v map[string]any) {
			ci := v["ci"].(map[string]any)
			ci["selection"] = "head"
			ci["selected_commit_sha"] = strings.Repeat("f", 40)
		},
		"CI future": func(v map[string]any) { v["ci"].(map[string]any)["observed_at"] = "2030-01-01T00:00:00Z" },
		"unresolved current": func(v map[string]any) {
			r := v["review_state"].(map[string]any)
			r["total_threads"] = 1
			r["unresolved_current"] = 1
		},
		"unresolved outdated": func(v map[string]any) {
			r := v["review_state"].(map[string]any)
			r["total_threads"] = 1
			r["unresolved_outdated"] = 1
		},
		"negative threads":          func(v map[string]any) { v["review_state"].(map[string]any)["total_threads"] = -1 },
		"threads overflow":          func(v map[string]any) { v["review_state"].(map[string]any)["total_threads"] = 10001 },
		"unknown gate":              func(v map[string]any) { v["review_state"].(map[string]any)["merge_state_status"] = "NEW_STATE" },
		"blocked":                   func(v map[string]any) { v["review_state"].(map[string]any)["merge_state_status"] = "BLOCKED" },
		"hooks":                     func(v map[string]any) { v["review_state"].(map[string]any)["merge_state_status"] = "HAS_HOOKS" },
		"unknown merge":             func(v map[string]any) { v["review_state"].(map[string]any)["mergeable"] = "UNKNOWN" },
		"draft":                     func(v map[string]any) { v["review_state"].(map[string]any)["draft"] = true },
		"closed":                    func(v map[string]any) { v["review_state"].(map[string]any)["pull_state"] = "CLOSED" },
		"changes requested":         func(v map[string]any) { v["review_state"].(map[string]any)["review_decision"] = "CHANGES_REQUESTED" },
		"review required":           func(v map[string]any) { v["review_state"].(map[string]any)["review_decision"] = "REVIEW_REQUIRED" },
		"wrong decision type":       func(v map[string]any) { v["review_state"].(map[string]any)["review_decision"] = []string{"APPROVED"} },
		"null review state":         func(v map[string]any) { v["review_state"] = nil },
		"missing nullable decision": func(v map[string]any) { delete(v["review_state"].(map[string]any), "review_decision") },
		"inventory spill":           func(v map[string]any) { v["review_state"].(map[string]any)["threads"] = []any{} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			_ = json.Unmarshal(valid, &value)
			mutate(value)
			body, _ := json.Marshal(value)
			response.Store(string(body))
			_, err := c.ReviewSourceReadiness(t.Context(), 2)
			checkError(t, err, "invalid_response")
		})
	}
	held := sampleReadiness()
	held.State = "held"
	held.Reasons = []string{"unresolved_outdated_threads"}
	held.ReviewState.TotalThreads = 1
	held.ReviewState.UnresolvedOutdated = 1
	body, _ := json.Marshal(held)
	response.Store(string(body))
	if got, err := c.ReviewSourceReadiness(t.Context(), 2); err != nil || got.State != "held" || got.ReviewState.UnresolvedOutdated != 1 {
		t.Fatal(got, err)
	}
	before := calls.Load()
	_, err := c.ReviewSourceReadiness(t.Context(), 0)
	checkError(t, err, "validation_failed")
	if calls.Load() != before {
		t.Fatal("invalid selector contacted service")
	}
}
