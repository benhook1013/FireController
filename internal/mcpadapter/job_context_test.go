package mcpadapter_test

import (
	"context"
	"encoding/json"
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

func TestJobContextToolsSDKOneSnapshotAndExplicitDisclosure(t *testing.T) {
	f := mutationFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); f.handler.ServeHTTP(w, r) }))
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": identity}, f.credential), "chosen")
	tools, err := allDiscoveredTools(t, session)
	if err != nil || len(tools.Tools) == 0 || tools.NextCursor != "" {
		t.Fatal("discovery", err)
	}
	found := false
	reads, writes := 0, 0
	for _, tool := range tools.Tools {
		if tool.Name != "job_context" {
			continue
		}
		found = true
		if tool.Annotations.ReadOnlyHint {
			reads++
		} else {
			writes++
		}

		var schema struct {
			Properties map[string]any
			Required   []string
			Additional bool `json:"additionalProperties"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) != 2 || len(schema.Required) != 1 || schema.Required[0] != "job_id" || schema.Additional || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatal("schema/annotations")
		}
	}
	if !found || reads != 1 || writes != 0 {
		t.Fatal("tool census", found, reads, writes)
	}
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "context-job", "title": "Other worker job", "brief": "Full authoritative brief", "worker_id": f.recipient}), &job)
	start := requests.Load()
	var got client.JobContextResult
	decodeResult(t, call(t, session, "job_context", map[string]any{"job_id": job.ID}), &got)
	if !got.Complete || got.Checkpoint != nil || got.Omitted == nil || len(got.Omitted) != 0 || got.WorkerID != f.sender || got.Job.WorkerID != f.recipient || requests.Load() != start+2 {
		t.Fatal("single endpoint/defaults", got, requests.Load()-start)
	}
	var note client.NoteReceipt
	decodeResult(t, call(t, session, "note_create", map[string]any{"idempotency_key": "context-note", "body": "Short pending preview"}), &note)
	var effects int
	if err = f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&effects); err != nil {
		t.Fatal(err)
	}
	start = requests.Load()
	decodeResult(t, call(t, session, "job_context", map[string]any{"job_id": job.ID, "limit": 1}), &got)
	if got.Complete || len(got.Omitted) != 1 || got.Omitted[0] != "note_bodies" || got.Notes.Items[0].BodyTruncated || got.Notes.Items[0].ID != note.ID || requests.Load() != start+2 {
		t.Fatal("preview/completeness/fanout", got, requests.Load()-start)
	}
	var after int
	if err = f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&after); err != nil || after != effects || f.providers.Load() != 1 {
		t.Fatal("context wrote/provider", err)
	}
}
func TestJobContextToolsStrictBeforeCredentialsAndCancellation(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": identity}, "missing-credential"), "alpha"), "")
	for _, raw := range []string{`{}`, `null`, `{"job_id":null}`, `{"job_id":"../job"}`, `{"job_id":"job","limit":0}`, `{"job_id":"job","limit":101}`, `{"job_id":"job","limit":null}`, `{"job_id":"job","limit":1.0}`, `{"job_id":"job","limit":1,"limit":2}`, `{"job_id":"job","worker_id":"other"}`, `{"job_id":"job","cursor":"next"}`, `{"Job_id":"job"}`, `{"job_id":"\ud800"}`} {
		diagnosticResult(t, call(t, session, "job_context", json.RawMessage(raw)), "invalid_arguments", false)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments contacted service")
	}
	f := mutationFixture(t)
	var reads atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			f.handler.ServeHTTP(w, r)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/context") {
			t.Error("fanout")
		}
		reads.Add(1)
		<-r.Context().Done()
	}))
	defer blocked.Close()
	a := f.adapter(t, blocked.URL, f.sender)
	a.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				ctx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
				defer cancel()
				return next(ctx, method, r)
			}
			return next(ctx, method, r)
		}
	})
	bounded := connect(t, a, "")
	started := time.Now()
	diagnosticResult(t, call(t, bounded, "job_context", map[string]any{"job_id": "job-one"}), "deadline_exceeded", false)
	if reads.Load() != 1 || time.Since(started) > time.Second {
		t.Fatal("cancellation/retry", reads.Load())
	}
}
