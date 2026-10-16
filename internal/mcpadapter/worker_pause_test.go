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

func TestWorkerPauseToolsSDKStdioReplayHistoryAndAnnotations(t *testing.T) {
	f := mutationFixture(t)
	var lose atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/pause") {
			writes.Add(1)
			if lose.CompareAndSwap(true, false) {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != 200 {
					t.Errorf("pause did not commit: %d", actual.Code)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(actual.Code)
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": identity}, f.credential), "chosen")
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 62 || tools.NextCursor != "" {
		t.Fatal("discovery", err)
	}
	want := map[string]int{"worker_pause_read": 0, "worker_pause_replace": 4, "worker_pause_history": 2}
	for _, tool := range tools.Tools {
		fields, ok := want[tool.Name]
		if !ok {
			continue
		}
		delete(want, tool.Name)
		var shape struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
			Additional bool           `json:"additionalProperties"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		write := tool.Name == "worker_pause_replace"
		if json.Unmarshal(raw, &shape) != nil || len(shape.Properties) != fields || shape.Additional || tool.Annotations.ReadOnlyHint == write || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || write && len(shape.Required) != 4 {
			t.Fatal("incorrect schema/annotations", tool.Name)
		}
	}
	if len(want) != 0 {
		t.Fatal(want)
	}
	var state client.WorkerPause
	decodeResult(t, call(t, session, "worker_pause_read", map[string]any{}), &state)
	if state.Revision != 0 || state.Paused || state.WorkerID != f.sender {
		t.Fatal(state)
	}
	stale := diagnosticResult(t, call(t, session, "worker_pause_replace", map[string]any{"idempotency_key": "zero-conflict", "expected_revision": 1, "paused": false, "reason": ""}), "worker_pause_revision_conflict", false)
	if details, ok := stale.Details.(map[string]any); !ok || details["current_revision"] != float64(0) {
		t.Fatal("zero revision lost", stale)
	}
	request := map[string]any{"idempotency_key": "pause-once", "expected_revision": 0, "paused": true, "reason": "exact\tPRIVATE_REASON\nnext"}
	before := writes.Load()
	lose.Store(true)
	diagnosticResult(t, call(t, session, "worker_pause_replace", request), "invalid_response", true)
	if writes.Load() != before+1 {
		t.Fatal("unknown write retried")
	}
	var original, replay client.WorkerPauseReceipt
	decodeResult(t, call(t, session, "worker_pause_replace", request), &original)
	if original.ID != f.sender || original.Revision != 1 {
		t.Fatal(original)
	}
	decodeResult(t, call(t, session, "worker_pause_replace", map[string]any{"idempotency_key": "resume-once", "expected_revision": 1, "paused": false, "reason": ""}), &replay)
	decodeResult(t, call(t, session, "worker_pause_replace", request), &replay)
	if replay != original {
		t.Fatal("receipt changed after resume", replay)
	}
	var effects int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	var history client.Page[client.WorkerPauseHistoryEntry]
	decodeResult(t, call(t, session, "worker_pause_history", map[string]any{"limit": 1}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 2 || history.Items[0].Paused || !history.HasMore {
		t.Fatal(history)
	}
	decodeResult(t, call(t, session, "worker_pause_history", map[string]any{"limit": 1, "cursor": history.Cursor}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 1 || history.Items[0].Reason != "exact\tPRIVATE_REASON\nnext" || history.HasMore {
		t.Fatal(history)
	}
	decodeResult(t, call(t, session, "worker_pause_read", map[string]any{}), &state)
	if state.Revision != 2 || state.Paused || state.Reason != "" {
		t.Fatal(state)
	}
	var after int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&after); err != nil || after != effects || f.providers.Load() != 1 {
		t.Fatal("reads mutated state or called provider", err)
	}
}

func TestWorkerPauseToolsStrictBeforeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": identity}, "missing-private-credential"), "alpha"), "")
	for _, v := range []struct{ name, raw string }{
		{"worker_pause_read", `{"worker_id":"other"}`}, {"worker_pause_read", `null`}, {"worker_pause_history", `{"limit":0}`}, {"worker_pause_history", `{"cursor":null}`}, {"worker_pause_history", `{"cursor":""}`},
		{"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"reason":""}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":null,"reason":""}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":false}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":false,"reason":null}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":false,"reason":"x"}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":true,"reason":"\r"}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":true,"reason":"\ud800"}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0.0,"paused":false,"reason":""}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":9223372036854775807,"paused":false,"reason":""}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":false,"reason":"","Paused":true}`}, {"worker_pause_replace", `{"idempotency_key":"k","expected_revision":0,"paused":false,"reason":"","reason":""}`},
	} {
		diagnosticResult(t, call(t, session, v.name, json.RawMessage(v.raw)), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid arguments used HTTP", requests.Load())
	}
}

func TestWorkerPauseToolsSharedDeadlineAndCancellation(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "five-second-budget", true: "owned-cancellation"}[short], func(t *testing.T) {
			f := mutationFixture(t)
			var reads atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					if !short {
						select {
						case <-time.After(150 * time.Millisecond):
						case <-r.Context().Done():
							return
						}
					}
					f.handler.ServeHTTP(w, r)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pause") {
					reads.Add(1)
					<-r.Context().Done()
					return
				}
				t.Error("unexpected fanout")
			}))
			defer service.Close()
			a := f.adapter(t, service.URL, f.sender)
			if short {
				a.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
						if method == "tools/call" {
							ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
							defer cancel()
							return next(ctx, method, request)
						}
						return next(ctx, method, request)
					}
				})
			}
			session := connect(t, a, "")
			started := time.Now()
			diagnosticResult(t, call(t, session, "worker_pause_read", map[string]any{}), "deadline_exceeded", false)
			elapsed := time.Since(started)
			if reads.Load() != 1 || !short && (elapsed < 4500*time.Millisecond || elapsed > 6*time.Second) || short && elapsed > time.Second {
				t.Fatal("deadline/cancellation", elapsed, reads.Load())
			}
		})
	}
}
