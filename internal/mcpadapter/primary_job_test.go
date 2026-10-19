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

func TestPrimaryJobToolsSDKStdioBoundReplayAndPages(t *testing.T) {
	f := mutationFixture(t)
	var lose atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/primary-job") {
			writes.Add(1)
			if lose.CompareAndSwap(true, false) {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != 200 {
					t.Errorf("focus did not commit: %d", actual.Code)
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
	tools, err := allDiscoveredTools(t, session)
	if err != nil || len(tools.Tools) == 0 || tools.NextCursor != "" {
		t.Fatal("discovery", err)
	}
	want := map[string]int{"job_primary_read": 0, "job_primary_replace": 3, "job_primary_history": 2}
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
		write := tool.Name == "job_primary_replace"
		if json.Unmarshal(raw, &shape) != nil || len(shape.Properties) != fields || shape.Additional || tool.Annotations.ReadOnlyHint == write || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != write || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || write && len(shape.Required) != 3 {
			t.Fatal("incorrect primary schema/annotations", tool.Name)
		}
	}
	if len(want) != 0 {
		t.Fatal("missing tools", want)
	}
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "primary-job", "title": "Independent focus", "worker_id": f.sender}), &job)
	var focus client.PrimaryJob
	decodeResult(t, call(t, session, "job_primary_read", map[string]any{}), &focus)
	if focus.Revision != 0 || focus.JobID != "" || focus.WorkerID != f.sender {
		t.Fatal(focus)
	}
	stale := diagnosticResult(t, call(t, session, "job_primary_replace", map[string]any{"idempotency_key": "initial-stale", "expected_revision": 1, "job_id": ""}), "primary_job_revision_conflict", false)
	details, ok := stale.Details.(map[string]any)
	if !ok || details["current_revision"] != float64(0) {
		t.Fatal("initial focus conflict lost revision zero", stale)
	}
	beforeLost := writes.Load()
	request := map[string]any{"idempotency_key": "primary-once", "expected_revision": 0, "job_id": job.ID}
	lose.Store(true)
	diagnosticResult(t, call(t, session, "job_primary_replace", request), "invalid_response", true)
	if writes.Load() != beforeLost+1 {
		t.Fatal("uncertain replacement automatically retried")
	}
	var receipt, replay client.PrimaryJobReceipt
	decodeResult(t, call(t, session, "job_primary_replace", request), &receipt)
	if receipt.ID != f.sender || receipt.Revision != 1 || receipt.Outcome != "updated" {
		t.Fatal(receipt)
	}
	decodeResult(t, call(t, session, "job_primary_replace", map[string]any{"idempotency_key": "primary-clear", "expected_revision": 1, "job_id": ""}), &replay)
	if replay.Revision != 2 {
		t.Fatal(replay)
	}
	decodeResult(t, call(t, session, "job_primary_replace", request), &replay)
	if replay != receipt {
		t.Fatal("original-key receipt changed after laterclear")
	}
	var events int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	var history client.Page[client.PrimaryJobHistoryEntry]
	decodeResult(t, call(t, session, "job_primary_history", map[string]any{"limit": 1}), &history)
	if len(history.Items) != 1 || history.Items[0].Reason != "cleared" || history.Items[0].JobID != "" || !history.HasMore {
		t.Fatal(history)
	}
	decodeResult(t, call(t, session, "job_primary_history", map[string]any{"limit": 1, "cursor": history.Cursor}), &history)
	if len(history.Items) != 1 || history.Items[0].JobID != job.ID || history.Items[0].ActorWorkerID != f.sender || history.HasMore {
		t.Fatal(history)
	}
	decodeResult(t, call(t, session, "job_primary_read", map[string]any{}), &focus)
	if focus.JobID != "" || focus.Revision != 2 {
		t.Fatal(focus)
	}
	var after int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&after); err != nil || after != events || f.providers.Load() != 1 {
		t.Fatal("read mutated state or calledprovider", err)
	}
}

func TestPrimaryJobToolsStrictArgumentsBeforeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": identity}, "missing-private-credential"), "alpha"), "")
	for _, v := range []struct{ name, raw string }{
		{"job_primary_read", `{"worker_id":"other"}`}, {"job_primary_read", `null`}, {"job_primary_history", `{"limit":0}`}, {"job_primary_history", `{"limit":101}`}, {"job_primary_history", `{"cursor":null}`}, {"job_primary_history", `{"cursor":""}`}, {"job_primary_history", `{"job_id":"job-one"}`},
		{"job_primary_replace", `{"idempotency_key":"k","expected_revision":0}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":0,"job_id":null}`}, {"job_primary_replace", `{"idempotency_key":"k","job_id":""}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":null,"job_id":""}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":-1,"job_id":""}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":9223372036854775807,"job_id":""}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":0,"job_id":"bad/job"}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":0,"Job_id":""}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":0,"job_id":"","job_id":""}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":0,"job_id":"","worker_id":"other"}`}, {"job_primary_replace", `{"idempotency_key":"k","expected_revision":0.0,"job_id":""}`},
	} {
		diagnosticResult(t, call(t, session, v.name, json.RawMessage(v.raw)), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid tools accessedHTTP", requests.Load())
	}
}

func TestPrimaryJobToolsSharedDeadlineAndOwnedCancellation(t *testing.T) {
	for _, mode := range []string{"five-second-budget", "canceled-context"} {
		t.Run(mode, func(t *testing.T) {
			f := mutationFixture(t)
			var reads atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					if mode == "five-second-budget" {
						select {
						case <-time.After(150 * time.Millisecond):
						case <-r.Context().Done():
							return
						}
					}
					f.handler.ServeHTTP(w, r)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/primary-job") {
					reads.Add(1)
					<-r.Context().Done()
					return
				}
				t.Error("unexpectedfanout")
			}))
			defer service.Close()
			a := f.adapter(t, service.URL, f.sender)
			if mode == "canceled-context" {
				a.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
						if method == "tools/call" {
							ctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
							defer cancel()
							return next(ctx, method, request)
						}
						return next(ctx, method, request)
					}
				})
			}
			session := connect(t, a, "")
			start := time.Now()
			diagnosticResult(t, call(t, session, "job_primary_read", map[string]any{}), "deadline_exceeded", false)
			elapsed := time.Since(start)
			if reads.Load() != 1 || mode == "five-second-budget" && (elapsed < 4500*time.Millisecond || elapsed > 6*time.Second) || mode == "canceled-context" && elapsed > time.Second {
				t.Fatal("shared deadline/cancellation failed", elapsed, reads.Load())
			}
		})
	}
}
