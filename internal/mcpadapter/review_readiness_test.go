package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewReadinessMCPActualPublicOptionsReplayAndLocalRead(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.RawQuery != "" || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("readiness SDK lost binding")
		}
		if r.Method == "POST" && r.Header.Get("Prefer") != "wait=0" {
			t.Error("immediate wait lost")
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	providers := f.providers.Load()
	ids := map[string]bool{}
	for _, option := range []struct {
		key           string
		ci, readiness bool
	}{{"source", false, false}, {"ci", true, false}, {"readiness", false, true}, {"both", true, true}} {
		args := sourceMCPArguments()
		args["idempotency_key"] = option.key
		args["wait_seconds"] = 0
		args["include_ci"] = option.ci
		args["include_readiness"] = option.readiness
		var receipt, replay, coalesced client.ReviewRequestReceipt
		result := call(t, session, "review_source_refresh", args)
		decodeResult(t, result, &receipt)
		data, _ := json.Marshal(result.StructuredContent)
		if len(data) > 1024 || receipt.State != "queued" || receipt.Revision != 1 || ids[receipt.ID] {
			t.Fatal("SDK option scope coalesced or receipt expanded", receipt)
		}
		ids[receipt.ID] = true
		decodeResult(t, call(t, session, "review_source_refresh", args), &replay)
		if replay != receipt {
			t.Fatal("SDK replay changed", replay)
		}
		args["idempotency_key"] = "compatible-" + option.key
		decodeResult(t, call(t, session, "review_source_refresh", args), &coalesced)
		if coalesced != receipt {
			t.Fatal("same scope did not coalesce", coalesced)
		}
		if option.key == "source" {
			args["idempotency_key"] = "omitted-options"
			delete(args, "include_ci")
			delete(args, "include_readiness")
			decodeResult(t, call(t, session, "review_source_refresh", args), &coalesced)
			if coalesced != receipt {
				t.Fatal("explicit false differs from omitted booleans")
			}
			args["idempotency_key"] = "source"
			args["include_readiness"] = true
			diagnosticResult(t, call(t, session, "review_source_refresh", args), "idempotency_conflict", false)
		}
		var operation client.ReviewSourceOperation
		decodeResult(t, call(t, session, "review_source_refresh_read", map[string]any{"refresh_id": receipt.ID}), &operation)
		if operation.IncludeCI != option.ci || operation.IncludeReadiness != option.readiness || operation.State != "queued" || operation.CI != nil || operation.ReviewState != nil || operation.ProjectID != f.project {
			t.Fatal("SDK operation changed requested booleans/invented facts", operation)
		}
	}
	var count, ciCount, readyCount int
	if err := f.store.DB().QueryRow(`SELECT count(*),sum(include_ci),sum(include_readiness) FROM review_source_refreshes`).Scan(&count, &ciCount, &readyCount); err != nil || count != 4 || ciCount != 2 || readyCount != 2 {
		t.Fatal("SDK option effects duplicated", count, ciCount, readyCount, err)
	}
	var events, receipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)`).Scan(&events, &receipts); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	diagnosticResult(t, call(t, session, "review_source_readiness", map[string]any{"pull_number": 2}), "reviews_readiness_unavailable", false)
	var afterEvents, afterReceipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM events),(SELECT count(*) FROM receipts)`).Scan(&afterEvents, &afterReceipts); err != nil || afterEvents != events || afterReceipts != receipts || requests.Load() != before+2 || f.providers.Load() != providers {
		t.Fatal("SDK local readiness read performed work", err)
	}
}

func mcpReadinessFixture(project string) client.ReviewSourceReadinessAssessment {
	target := client.ReviewSourceObservation{RepositoryID: 1, RepositoryName: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ParentSHA: strings.Repeat("b", 40), ObservedAt: "2026-10-16T00:00:00Z"}
	return client.ReviewSourceReadinessAssessment{ProjectID: project, RefreshID: mcpRefreshID, Source: "observed_comparison", ConfigurationRevision: 1, PolicyRevision: 1, SourceObservedAt: target.ObservedAt, EvaluatedAt: target.ObservedAt, Target: target, State: "ready_as_observed", Reasons: []string{}, Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli", Covered: true}}, CI: client.ReviewSourceCISummary{State: "satisfied", Selection: "test_merge", SelectedCommitSHA: strings.Repeat("c", 40), MergeState: "verified", ObservedAt: target.ObservedAt, Required: 1, Satisfied: 1}, ReviewState: client.ReviewSourceReviewStateSummary{State: "complete", ObservedAt: target.ObservedAt, PullState: "OPEN", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", TotalThreads: 3}}
}

// Controlled compact facts test SDK/adapter projection, not provider correctness.
func TestReviewReadinessMCPControlledResultsAndWaitPreferences(t *testing.T) {
	binding := localContext()
	assessment := mcpReadinessFixture(binding.ProjectID)
	operation := client.ReviewSourceOperation{ID: mcpRefreshID, ProjectID: binding.ProjectID, PullNumber: 2, HeadSHA: assessment.Target.HeadSHA, ConfigurationRevision: 1, State: "completed", Revision: 3, CreatedAt: assessment.Target.ObservedAt, UpdatedAt: assessment.Target.ObservedAt, Observation: &assessment.Target, IncludeReadiness: true, CI: &assessment.CI, ReviewState: &assessment.ReviewState}
	var mode atomic.Int64
	var requests atomic.Int64
	var expectedWait atomic.Int64
	expectedWait.Store(2)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != binding.InstanceID || r.Header.Get("X-FC-Worker") != binding.WorkerID || r.URL.RawQuery != "" {
			t.Error("readiness SDK wire call lost binding")
		}
		base := "/api/v1/projects/" + binding.ProjectID + "/reviews/"
		switch r.URL.Path {
		case base + "source-refreshes":
			var input client.ReviewSourceRequest
			if json.NewDecoder(r.Body).Decode(&input) != nil || !input.IncludeReadiness || input.IncludeCI || input.PullNumber != 2 || input.HeadSHA != assessment.Target.HeadSHA || input.ExpectedConfigurationRevision != 1 || r.Method != "POST" || r.Header.Get("Prefer") != "wait="+strconv.FormatInt(expectedWait.Load(), 10) || r.Header.Get("Idempotency-Key") != "source-once" {
				t.Error("SDK readiness request/default CI/wait changed")
			}
			if expectedWait.Load() == 0 {
				w.WriteHeader(202)
				_ = json.NewEncoder(w).Encode(client.ReviewRequestReceipt{ID: mcpRefreshID, State: "queued", Revision: 1})
			} else {
				_ = json.NewEncoder(w).Encode(operation)
			}
		case base + "source/2/readiness":
			if r.Method != "GET" || r.Header.Get("Prefer") != "" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("SDK read requested provider work")
			}
			value := assessment
			switch mode.Load() {
			case 1:
				value.State = "held"
				value.Reasons = []string{"unresolved_outdated_threads"}
				value.ReviewState.UnresolvedOutdated = 1
			case 2:
				value.State = "held"
				value.Reasons = []string{"ci_merge_state_unproven"}
				value.CI = client.ReviewSourceCISummary{State: "unproven", Reason: "ci_candidate_unproven", MergeState: "unproven", ObservedAt: value.Target.ObservedAt}
			}
			_ = json.NewEncoder(w).Encode(value)
		default:
			t.Error("unexpected readiness SDK route", r.URL.Path)
		}
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	for _, wait := range []int{-1, 0, 10} {
		args := sourceMCPArguments()
		args["include_readiness"] = true
		if wait < 0 {
			expectedWait.Store(2)
		} else {
			expectedWait.Store(int64(wait))
			args["wait_seconds"] = wait
		}
		result := call(t, session, "review_source_refresh", args)
		data, _ := json.Marshal(result.StructuredContent)
		if len(data) > 16<<10 || strings.Contains(string(data), `"operation":`) || strings.Contains(string(data), `"threads":`) || strings.Contains(string(data), `"checks":`) {
			t.Fatal("SDK result expanded/wrapped", string(data))
		}
		if wait == 0 {
			var receipt client.ReviewRequestReceipt
			decodeResult(t, result, &receipt)
			if receipt.State != "queued" {
				t.Fatal(receipt)
			}
		} else {
			var got client.ReviewSourceOperation
			decodeResult(t, result, &got)
			if got.State != "completed" || !got.IncludeReadiness || got.IncludeCI || got.CI == nil || got.ReviewState == nil {
				t.Fatal(got)
			}
		}
	}
	for i := int64(0); i < 3; i++ {
		mode.Store(i)
		result := call(t, session, "review_source_readiness", map[string]any{"pull_number": 2})
		var got client.ReviewSourceReadinessAssessment
		decodeResult(t, result, &got)
		data, _ := json.Marshal(result.StructuredContent)
		if got.Target != assessment.Target || got.Source != "observed_comparison" || len(data) > 16<<10 || strings.Contains(string(data), `"threads":`) || strings.Contains(string(data), `"checks":`) || (got.State == "ready_as_observed") != (i == 0) {
			t.Fatal("SDK lost held/observed compact facts", got)
		}
		if i == 1 && got.ReviewState.UnresolvedOutdated != 1 || i == 2 && got.CI.MergeState != "unproven" {
			t.Fatal(got)
		}
	}
	if requests.Load() != 12 {
		t.Fatal("SDK readiness tools polled", requests.Load())
	}
}

func TestReviewReadinessMCPDiscoveryAndInvalidArgumentsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-readiness-credential"), "chosen")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	foundRead, foundFlag := false, false
	for _, tool := range listed.Tools {
		if tool.Name != "review_source_readiness" && tool.Name != "review_source_refresh" {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
			Additional bool                       `json:"additionalProperties"`
		}
		data, _ := json.Marshal(tool.InputSchema)
		if json.Unmarshal(data, &schema) != nil || schema.Additional {
			t.Fatal("readiness schema widened")
		}
		if tool.Name == "review_source_readiness" {
			foundRead = true
			a := tool.Annotations
			if len(schema.Properties) != 1 || len(schema.Required) != 1 || schema.Required[0] != "pull_number" || a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Fatal("readiness read schema/effects incorrect")
			}
		} else {
			foundFlag = true
			var flag struct {
				Type    string `json:"type"`
				Default bool   `json:"default"`
			}
			if len(schema.Properties) != 7 || len(schema.Required) != 4 || json.Unmarshal(schema.Properties["include_readiness"], &flag) != nil || flag.Type != "boolean" || flag.Default {
				t.Fatal("readiness optional boolean missing/mandatory")
			}
		}
	}
	if !foundRead || !foundFlag || listed.NextCursor != "" {
		t.Fatal("readiness discovery incomplete")
	}
	for _, value := range []any{nil, "true", 1, []any{}, map[string]any{}} {
		args := sourceMCPArguments()
		args["include_readiness"] = value
		diagnosticResult(t, call(t, session, "review_source_refresh", args), "invalid_arguments", false)
	}
	args := sourceMCPArguments()
	data, _ := json.Marshal(args)
	base := strings.TrimSuffix(string(data), "}")
	for _, tail := range []string{`,"include_readiness":true,"include_readiness":false}`, `,"Include_Readiness":true}`, `,"includeReadiness":true}`, `,"readiness":true}`} {
		diagnosticResult(t, call(t, session, "review_source_refresh", json.RawMessage(base+tail)), "invalid_arguments", false)
	}
	for _, args := range []any{nil, map[string]any{}, map[string]any{"pull_number": nil}, map[string]any{"pull_number": 0}, map[string]any{"pull_number": "2"}, map[string]any{"pull_number": 2, "include_readiness": true}, map[string]any{"pull_number": 2, "refresh": true}, map[string]any{"pull_number": 2, "project_id": "other"}, json.RawMessage(`{"pull_number":2,"pull_number":3}`), json.RawMessage(`{"Pull_Number":2}`)} {
		diagnosticResult(t, call(t, session, "review_source_readiness", args), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid readiness SDK args/discovery reached credentials/HTTP")
	}
}
