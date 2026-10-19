package mcpadapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpRefreshID = "review_refresh_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sourceMCPArguments() map[string]any {
	return map[string]any{"pull_number": 2, "head_sha": strings.Repeat("a", 40), "expected_configuration_revision": 1, "idempotency_key": "source-once"}
}

func TestReviewSourceMCPDiscoveryAndInvalidArgumentsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-source-credential"), "chosen")
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"review_source_refresh": 7, "review_source_refresh_read": 1, "review_source_policy_assess": 1, "review_source_ci_assess": 1}
	for _, tool := range listed.Tools {
		properties, ok := want[tool.Name]
		if !ok {
			continue
		}
		delete(want, tool.Name)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
			Additional bool           `json:"additionalProperties"`
		}
		data, _ := json.Marshal(tool.InputSchema)
		write := tool.Name == "review_source_refresh"
		required := properties
		if write {
			required -= 3
		}
		if json.Unmarshal(data, &schema) != nil || schema.Additional || len(schema.Properties) != properties || len(schema.Required) != required {
			t.Fatal("source schema widened or lost requiredfields", tool.Name)
		}
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint == write || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint != write {
			t.Fatal("source effects annotations incorrect", tool.Name)
		}
	}
	if len(want) != 0 || listed.NextCursor != "" {
		t.Fatal("source discovery incomplete", want)
	}
	valid := map[string]map[string]any{"review_source_refresh": sourceMCPArguments(), "review_source_refresh_read": {"refresh_id": mcpRefreshID}, "review_source_policy_assess": {"pull_number": 2}}
	for name, args := range valid {
		bad := func(value any) {
			t.Helper()
			diagnosticResult(t, call(t, session, name, value), "invalid_arguments", false)
		}
		bad(json.RawMessage(`null`))
		bad(json.RawMessage(`[]`))
		for key := range args {
			copy := map[string]any{}
			for k, v := range args {
				if k != key {
					copy[k] = v
				}
			}
			bad(copy)
			copy[key] = nil
			bad(copy)
		}
		for _, field := range []string{"project_id", "worker_id", "profile", "refresh", "extra"} {
			copy := map[string]any{}
			for k, v := range args {
				copy[k] = v
			}
			copy[field] = "other"
			bad(copy)
		}
		data, _ := json.Marshal(args)
		text := string(data)
		for field := range args {
			bad(json.RawMessage(strings.Replace(text, `"`+field+`":`, `"`+strings.ToUpper(field)+`":`, 1)))
		}
		if name == "review_source_refresh" {
			for _, value := range []any{nil, -1, 51, 60, 61, 1.5, "2"} {
				copy := sourceMCPArguments()
				copy["wait_seconds"] = value
				bad(copy)
			}
			for _, field := range []string{"head_sha", "idempotency_key"} {
				copy := sourceMCPArguments()
				copy[field] = "bad\n"
				bad(copy)
			}
			bad(json.RawMessage(strings.Replace(text, `"pull_number":2`, `"pull_number":2,"pull_number":2`, 1)))
		} else if name == "review_source_refresh_read" {
			bad(map[string]any{"refresh_id": "review_operation_" + strings.Repeat("a", 32)})
		}
	}
	if requests.Load() != 0 {
		t.Fatal("discovery/badargs loaded credentials or HTTP")
	}
}

func TestReviewSourceMCPActualPublicEnqueueUnknownReplayAndRead(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var requests, posts atomic.Int64
	var drop atomic.Bool
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender || r.URL.RawQuery != "" {
			t.Error("source MCP lost binding")
		}
		if r.Method == "POST" {
			posts.Add(1)
			if r.Header.Get("Prefer") != "wait=0" {
				t.Error("explicit wait lost")
			}
			if drop.Swap(false) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				if recorder.Code != 202 {
					t.Errorf("request did not commit: %d %s", recorder.Code, recorder.Body.String())
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = connection.Close()
				}
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	args := sourceMCPArguments()
	args["wait_seconds"] = 0
	drop.Store(true)
	diagnosticResult(t, call(t, session, "review_source_refresh", args), "connection_failed", true)
	var receipt, replay client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_source_refresh", args), &receipt)
	decodeResult(t, call(t, session, "review_source_refresh", args), &replay)
	if receipt != replay || receipt.State != "queued" || receipt.Revision != 1 || posts.Load() != 3 {
		t.Fatal("source replay changed receipt", receipt, replay)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_source_refreshes`).Scan(&count); err != nil || count != 1 {
		t.Fatal("unknownresponse retry created another sourceoperation", count, err)
	}
	providerBefore := f.providers.Load()
	before := requests.Load()
	var operation client.ReviewSourceOperation
	decodeResult(t, call(t, session, "review_source_refresh_read", map[string]any{"refresh_id": receipt.ID}), &operation)
	if operation.ID != receipt.ID || operation.State != "queued" || operation.ProjectID != f.project || operation.Observation != nil || requests.Load() != before+2 || f.providers.Load() != providerBefore {
		t.Fatal("operationread refreshed/polled or lost binding", operation)
	}
	diagnosticResult(t, call(t, session, "review_source_policy_assess", map[string]any{"pull_number": 2}), "reviews_source_unavailable", false)
}

func TestReviewSourceMCPDirectTerminalAndAssessmentAreFlat(t *testing.T) {
	binding := localContext()
	observation := client.ReviewSourceObservation{RepositoryID: 1, RepositoryName: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ParentSHA: strings.Repeat("b", 40), ObservedAt: "2026-10-16T00:00:00Z"}
	operation := client.ReviewSourceOperation{ID: mcpRefreshID, ProjectID: binding.ProjectID, PullNumber: 2, HeadSHA: observation.HeadSHA, ConfigurationRevision: 1, State: "completed", Revision: 3, CreatedAt: observation.ObservedAt, UpdatedAt: observation.ObservedAt, Observation: &observation}
	assessment := client.ReviewSourcePolicyAssessment{ProjectID: binding.ProjectID, RefreshID: mcpRefreshID, Source: "observed_comparison", ConfigurationRevision: 1, PolicyRevision: 1, SourceObservedAt: observation.ObservedAt, EvaluatedAt: observation.ObservedAt, State: "held", Target: observation, Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli"}}}
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Method == "POST" {
			if r.Header.Get("Prefer") != "wait=2" {
				t.Error("defaultwait missing")
			}
			_ = json.NewEncoder(w).Encode(operation)
		} else {
			_ = json.NewEncoder(w).Encode(assessment)
		}
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	var got client.ReviewSourceOperation
	result := call(t, session, "review_source_refresh", sourceMCPArguments())
	decodeResult(t, result, &got)
	if got.ID != operation.ID || got.State != "completed" {
		t.Fatal("direct terminal was wrapped", got)
	}
	var current client.ReviewSourcePolicyAssessment
	decodeResult(t, call(t, session, "review_source_policy_assess", map[string]any{"pull_number": 2}), &current)
	if current.Source != "observed_comparison" || current.ProjectID != binding.ProjectID || requests.Load() != 4 {
		t.Fatal("assessment changed binding or polled", current)
	}
}

func TestReviewSourceMCPCancellationReleasesWait(t *testing.T) {
	binding := localContext()
	entered, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	shutdown := make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-shutdown:
		}
		closed <- struct{}{}
	}))
	defer service.Close()
	defer close(shutdown)
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	args := sourceMCPArguments()
	args["wait_seconds"] = 50
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "review_source_refresh", Arguments: args})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sourcewait didnotenterHTTP")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SDKwaitignoredcancellation")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("SDKcancellationleftHTTPopen")
	}
	var resumed profile.Context
	decodeResult(t, call(t, session, "context", nil), &resumed)
	if resumed.ProjectID != binding.ProjectID {
		t.Fatal("cancellationchangedbinding")
	}
}
