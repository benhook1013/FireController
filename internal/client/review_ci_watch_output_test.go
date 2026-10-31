package client_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestAgentCIWatchExplainsControllerAndGitHubPendingWithoutChangingFacts(t *testing.T) {
	for _, test := range []struct {
		name, reason, observed, phase, explanation string
		required, satisfied, pending, unproven     int64
	}{
		{"not observed", "ci_watch_check_pending", "", "not_observed", "mean unknown", 0, 0, 0, 0},
		{"refresh zero", "ci_watch_refresh_pending", "", "refreshing", "newer controller check", 0, 0, 0, 0},
		{"refresh earlier green", "ci_watch_refresh_pending", "2026-11-01T00:00:00Z", "refreshing", "earlier counts and checks are informational", 1, 1, 0, 0},
		{"GitHub pending", "ci_required_checks_pending", "2026-11-01T00:00:00Z", "checks_pending", "GitHub checks still pending as of observed_at", 1, 0, 1, 0},
		{"missing evidence", "ci_required_checks_unproven", "2026-11-01T00:00:00Z", "checks_unproven", "missing required-check evidence", 1, 0, 0, 1},
		{"unavailable", "ci_watch_source_unavailable", "", "observation_unproven", "not established a current required-CI result", 0, 0, 0, 0},
		{"coherence changed", "ci_observation_changed", "2026-11-01T00:00:00Z", "observation_unproven", "collected counts remain informational", 1, 1, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := client.CIWatchTargetResult{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), State: "pending", Reason: test.reason, ObservedAt: test.observed, Required: test.required, Satisfied: test.satisfied, Pending: test.pending, Unproven: test.unproven, Checks: []client.ReviewCIContext{}}
			if test.required > 0 {
				state := "satisfied"
				if test.pending > 0 {
					state = "pending"
				}
				if test.unproven > 0 {
					state = "unproven"
				}
				item.Checks = []client.ReviewCIContext{{Context: "Required build", State: state, Reason: "original_reason"}}
			}
			original := client.CIWatchResult{ProjectID: "original_project", State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{item}}
			before, _ := json.Marshal(original)
			out := client.AgentCIWatch(original)
			if !reflect.DeepEqual(out.CIWatchResult, original) || len(out.Targets) != 1 || !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, item) || out.Targets[0].Phase != test.phase || out.Targets[0].Diagnostic == nil || !strings.Contains(out.Targets[0].Diagnostic.Explanation, test.explanation) {
				t.Fatal("wrong controller/GitHub distinction or changed raw result", out)
			}
			if !strings.Contains(out.Targets[0].Diagnostic.NextAction, "another bounded CI wait") {
				t.Fatal("invented surviving watcher or missing supported followup", out)
			}
			encoded, _ := json.Marshal(out)
			var got, want map[string]any
			_ = json.Unmarshal(encoded, &got)
			_ = json.Unmarshal(before, &want)
			for _, v := range got["targets"].([]any) {
				target := v.(map[string]any)
				delete(target, "phase")
				delete(target, "diagnostic")
				if _, exists := target["next_check_at"]; exists {
					t.Fatal("invented schedule metadata")
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("projection changed original wire facts", string(encoded))
			}
			after, _ := json.Marshal(original)
			if string(before) != string(after) {
				t.Fatal("projection mutated input")
			}
		})
	}
	for _, state := range []string{"passed", "failed", "superseded", "unconfigured", "closed"} {
		out := client.AgentCIWatch(client.CIWatchResult{State: state, Targets: []client.CIWatchTargetResult{{State: state, Reason: "ci_watch_check_pending"}}})
		if out.Targets[0].Diagnostic != nil || out.Targets[0].Phase != "" {
			t.Fatal("terminal state reclassified as acquisition delay", out)
		}
	}
}

func TestAgentCIWatchUsesUnchangedValidatedReadTransport(t *testing.T) {
	pins := ciWatchPins()[:1]
	raw := client.CIWatchResult{ProjectID: "project-one", State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{{PullNumber: pins[0].PullNumber, HeadSHA: pins[0].HeadSHA, BaseSHA: pins[0].BaseSHA, State: "pending", Reason: "ci_merge_base_mismatch", Checks: []client.ReviewCIContext{}}}}
	posts := 0
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts++
		body, err := io.ReadAll(r.Body)
		var input struct {
			Targets []client.CIWatchTarget `json:"targets"`
		}
		if err != nil || json.Unmarshal(body, &input) != nil || !reflect.DeepEqual(input.Targets, pins) || r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.Header.Get("Prefer") != "wait=60" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-CI-Watch-Observation") != "" {
			t.Error("output helper changed transport or source pins")
		}
		write(w, 200, raw)
	}))
	defer service.Close()
	c := newClient(t, service)
	value, err := c.WatchCI(t.Context(), pins, 60)
	if err != nil || !reflect.DeepEqual(value, raw) {
		t.Fatal("strict typed read changed", value, err)
	}
	out := client.AgentCIWatch(value)
	if posts != 1 || out.Targets[0].Phase != "observation_unproven" || out.Targets[0].Diagnostic == nil || !strings.Contains(out.Targets[0].Diagnostic.NextAction, "reconcile the PR branch") {
		t.Fatal("output adaptation requested extra reads or failed", out, posts)
	}
}

func TestAgentCIWatchReusesCanonicalMergeRefusalGuidance(t *testing.T) {
	for _, reason := range []string{"ci_merge_base_mismatch", "ci_merge_identity_unproven", "ci_merge_pending"} {
		t.Run(reason, func(t *testing.T) {
			item := client.CIWatchTargetResult{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ObservedHeadSHA: strings.Repeat("a", 40), ObservedBaseSHA: strings.Repeat("c", 40), State: "pending", Reason: reason, Checks: []client.ReviewCIContext{}, ObservedAt: "2026-11-01T00:00:00Z"}
			input := client.CIWatchResult{ProjectID: "project-one", State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{item}}
			before, _ := json.Marshal(input)
			canonical := client.AgentReviewCI(client.ReviewSourceCIAssessment{State: "unproven", Reason: reason}).Diagnostic
			out := client.AgentCIWatch(input)
			diagnostic := out.Targets[0].Diagnostic
			if canonical == nil || diagnostic == nil || diagnostic.Explanation != canonical.Explanation || diagnostic.NextAction != canonical.NextAction || out.Targets[0].Phase != "observation_unproven" {
				t.Fatal("watch lost canonical actionable comparison guidance", out)
			}
			if !reflect.DeepEqual(out.Targets[0].CIWatchTargetResult, item) {
				t.Fatal("guidance changed source facts")
			}
			text := diagnostic.Explanation + diagnostic.NextAction
			if strings.Contains(text, item.BaseSHA) || strings.Contains(text, item.ObservedBaseSHA) {
				t.Fatal("PR base values invented stale merge-parent/live-tip diagnostics")
			}
			if reason == "ci_merge_base_mismatch" && (!strings.Contains(diagnostic.NextAction, "reconcile the PR branch") || strings.Contains(diagnostic.NextAction, "Start another bounded CI wait")) {
				t.Fatal("known mismatch still offers only another wait", diagnostic)
			}
			after, _ := json.Marshal(input)
			if string(before) != string(after) {
				t.Fatal("output mutated original response")
			}
		})
	}
	// Acquisition state remains authoritative over stale check-level reasons.
	for _, reason := range []string{"ci_watch_check_pending", "ci_watch_refresh_pending"} {
		item := client.CIWatchTargetResult{State: "pending", Reason: reason, Checks: []client.ReviewCIContext{{Context: "old", State: "unproven", Reason: "ci_merge_base_mismatch"}}}
		out := client.AgentCIWatch(client.CIWatchResult{State: "pending", Targets: []client.CIWatchTargetResult{item}})
		if out.Targets[0].Diagnostic == nil || strings.Contains(out.Targets[0].Diagnostic.Explanation, "test merge") || !strings.Contains(out.Targets[0].Diagnostic.NextAction, "another bounded CI wait") {
			t.Fatal("old comparison refusal overrode current controller refresh", out)
		}
	}
	for _, state := range []string{"passed", "failed", "closed", "superseded", "unconfigured"} {
		out := client.AgentCIWatch(client.CIWatchResult{State: state, Targets: []client.CIWatchTargetResult{{State: state, Reason: "ci_merge_base_mismatch"}}})
		if out.Targets[0].Diagnostic != nil {
			t.Fatal("terminal result gained a pending refusal", out)
		}
	}
}
