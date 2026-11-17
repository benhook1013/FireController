package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReviewStartMCPStrictArgumentsAndDiscovery(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-selection-credential"), "chosen")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range listed.Tools {
		if tool.Name != "review_start" && tool.Name != "review_selection_read" {
			continue
		}
		seen[tool.Name] = true
		write := tool.Name == "review_start"
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint == write || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint != write {
			t.Fatal("selection effects incorrectly advertised", tool.Name)
		}
	}
	if len(seen) != 2 {
		t.Fatal("selection tools missing")
	}
	for name, rows := range map[string][]string{
		"review_start":          {`{}`, `null`, `[]`, `{"channel":"cli"}`, `{"channel":"cli","idempotency_key":"once","expected_pull_number":null}`, `{"channel":"cli","idempotency_key":"once","expected_configuration_revision":0}`, `{"channel":"cli","idempotency_key":"once","expected_configuration_revision":9223372036854775807}`, `{"channel":"cli","idempotency_key":"once","expected_pull_number":1.5}`, `{"channel":"cli","idempotency_key":"once","wait_seconds":901}`, `{"channel":"cli","idempotency_key":"once","channel":"hosted"}`, `{"channel":"cli","idempotency_key":"once","worker_id":"other"}`, `{"channel":"CLI","idempotency_key":"once"}`},
		"review_selection_read": {`{}`, `{"selection_id":"review_run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, `{"selection_id":"review_selection_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","wait_seconds":-1}`, `{"selection_id":"review_selection_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project_id":"other"}`},
	} {
		for _, raw := range rows {
			diagnosticResult(t, call(t, session, name, json.RawMessage(raw)), "invalid_arguments", false)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid selection arguments loaded credentials or contacted service")
	}
}

func TestReviewStartMCPActualReplayAndBackgroundWait(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var posts atomic.Int64
	started, detached := make(chan struct{}, 1), make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selection := strings.Contains(r.URL.Path, "/reviews/selections")
		if selection && r.Method == "POST" {
			posts.Add(1)
		}
		waiting := selection && r.Method == "GET" && boundedPositiveReviewWait(t, r.Header.Get("Prefer"))
		if waiting {
			started <- struct{}{}
		}
		handler.ServeHTTP(w, r)
		if waiting {
			detached <- struct{}{}
		}
	}))
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	before := f.providers.Load()
	args := map[string]any{"channel": "cli", "idempotency_key": "one-channel-selection", "expected_pull_number": 2, "wait_seconds": 0}
	var first, replay client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_start", args), &first)
	decodeResult(t, call(t, session, "review_start", args), &replay)
	if first != replay || first.Revision != 1 || first.State != "queued" || client.ValidateReviewRunSelectionID(first.ID) != nil {
		t.Fatal("selection replay lost original identity")
	}
	var state client.ReviewRunState
	decodeResult(t, call(t, session, "review_selection_read", map[string]any{"selection_id": first.ID}), &state)
	if state.Selection.ID != first.ID || state.Selection.WorkerID != f.sender || state.Selection.ExpectedPullNumber != 2 || state.Selection.State != "queued" || state.Operation != nil {
		t.Fatal("queued selection fabricated a target or operation")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "review_selection_read", Arguments: map[string]any{"selection_id": first.ID, "wait_seconds": 50}})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("selection wait did not reach service")
	}
	var foreground struct {
		profile.Context
		Verified bool `json:"verified"`
	}
	decodeResult(t, call(t, session, "doctor", nil), &foreground)
	if !foreground.Verified || foreground.WorkerID != f.sender {
		t.Fatal("foreground read lost binding")
	}
	select {
	case err := <-done:
		t.Fatal("selection wait ended at ordinary tool deadline", err)
	case <-time.After(5100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("selection wait cancellation did not return")
	}
	select {
	case <-detached:
	case <-time.After(2 * time.Second):
		t.Fatal("selection wait remained attached")
	}
	decodeResult(t, call(t, session, "review_selection_read", map[string]any{"selection_id": first.ID}), &state)
	var selections, operations int
	if err := f.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM review_selections`).Scan(&selections); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM review_operations`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if selections != 1 || operations != 1 || posts.Load() != 2 || f.providers.Load() != before || state.Selection.State != "queued" {
		t.Fatal("wait/replay changed or retried provider work")
	}
}

func TestReviewSelectionMCPSourceHoldExplainsOriginalReadOnlyResult(t *testing.T) {
	binding := localContext()
	value := client.ReviewRunState{Selection: client.ReviewRunSelection{ID: "review_selection_" + strings.Repeat("a", 32), ProjectID: binding.ProjectID, ActorID: "principal_original", WorkerID: binding.WorkerID, Channel: "cli", ConfigurationRevision: 7, PolicyRevision: 2, ExpectedPullNumber: 3365, State: "held", Reason: "source_unavailable", Revision: 3, CreatedAt: "2026-10-18T01:00:00Z", UpdatedAt: "2026-10-18T01:01:00Z"}}
	var reads, unexpected atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" && r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+binding.ProjectID+"/reviews/selections/"+value.Selection.ID || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
			unexpected.Add(1)
			w.WriteHeader(500)
			return
		}
		reads.Add(1)
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	var out client.ReviewSelectionOutput
	decodeResult(t, call(t, session, "review_selection_read", map[string]any{"selection_id": value.Selection.ID}), &out)
	if !reflect.DeepEqual(out.ReviewRunState, value) || out.Operation != nil || out.Diagnostic == nil || !strings.Contains(out.Diagnostic.Explanation, "did not retain its exact failure cause") || !strings.Contains(out.Diagnostic.NextAction, "old key replays") || reads.Load() != 1 || unexpected.Load() != 0 {
		t.Fatal("SDK hold invented cause, changed facts or requested work", out)
	}
}
