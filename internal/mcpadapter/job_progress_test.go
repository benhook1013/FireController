package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestUnifiedJobToolsRecoverCLICommittedResponseAndPartialProgress(t *testing.T) {
	f := mutationFixture(t)
	var corrupt atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" && strings.Contains(r.URL.Path, "/jobs/") {
			writes.Add(1)
			if corrupt.CompareAndSwap(true, false) {
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, r)
				if rec.Code != 200 {
					t.Error("write did not commit")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"id":"incomplete"}`))
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	binding.InstanceID = f.store.InstanceID()
	binding.ProjectID = f.project
	binding.WorkerID = f.sender
	path := profiles(t, map[string]profile.Context{"chosen": binding}, f.credential)
	session := connect(t, adapter(t, path, "chosen"), "")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "setup-current", "title": "Job", "brief": "Standing brief"}), &job)
	corrupt.Store(true)
	var out, stderr bytes.Buffer
	code := cli.Run([]string{"--profiles-file", path, "--profile", "chosen", "jobs", "update", job.ID, "--expect-revision", "1", "--key", "original-key", "--progress", "PRIVATE_PROGRESS", "--evidence", "proof:one"}, nil, &out, &stderr)
	if code == cli.ExitSuccess || out.Len() != 0 || !strings.Contains(stderr.String(), "outcome_unknown") {
		t.Fatal("lost committed response not uncertain")
	}
	request := map[string]any{"job_id": job.ID, "idempotency_key": "original-key", "expected_revision": 1, "progress": map[string]any{"summary": "PRIVATE_PROGRESS", "evidence": []string{"proof:one"}}}
	var original client.JobReceipt
	decodeResult(t, call(t, session, "job_update", request), &original)
	if original.Revision != 2 || original.Outcome != "updated" || writes.Load() != 2 {
		t.Fatal("cross-adapter recovery retried or duplicated")
	}
	var correction client.JobReceipt
	decodeResult(t, call(t, session, "job_update", map[string]any{"job_id": job.ID, "idempotency_key": "correction", "expected_revision": 2, "status": "blocked", "progress": map[string]any{"next": "Next", "evidence": []string{}}}), &correction)
	var replay client.JobReceipt
	decodeResult(t, call(t, session, "job_update", request), &replay)
	if replay != original {
		t.Fatal("later edit changed original receipt")
	}
	var current client.Job
	decodeResult(t, call(t, session, "job_read", map[string]any{"job_id": job.ID}), &current)
	if current.Revision != 3 || current.Progress.Summary != "PRIVATE_PROGRESS" || current.Progress.Next != "Next" || len(current.Progress.Evidence) != 0 || current.Brief != "Standing brief" || current.Notes.Items == nil || current.Checklist.Remaining == nil {
		t.Fatal("default job state incomplete")
	}
	var page client.Page[client.ProgressHistoryEntry]
	decodeResult(t, call(t, session, "job_progress_history", map[string]any{"job_id": job.ID, "limit": 1}), &page)
	if len(page.Items) != 1 || !page.HasMore || page.Items[0].JobRevision != 3 {
		t.Fatal("archive was not bounded")
	}
	var noop client.JobReceipt
	decodeResult(t, call(t, session, "job_update", map[string]any{"job_id": job.ID, "idempotency_key": "noop", "expected_revision": 3, "status": "blocked"}), &noop)
	if noop.Revision != 3 || noop.Outcome != "unchanged" {
		t.Fatal("noop revision changed")
	}
	if f.providers.Load() != 1 {
		t.Fatal("unexpected provider work")
	}
}
func TestUnifiedJobToolsRejectBeforeCredentialAndNetwork(t *testing.T) {
	var count atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { count.Add(1) }))
	defer s.Close()
	binding := localContext()
	binding.ServerURL = s.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": binding}, "missing-credential"), "alpha"), "")
	base := `{"job_id":"job-one","idempotency_key":"key","expected_revision":1,"progress":{"summary":"ok"}}`
	cases := []string{`{}`, strings.Replace(base, `"expected_revision":1`, `"expected_revision":0`, 1), strings.Replace(base, `"summary":"ok"`, `"summary":null`, 1), strings.Replace(base, `"summary":"ok"`, `"evidence":null`, 1), strings.Replace(base, `"summary":"ok"`, `"summary":"one","summary":"two"`, 1), strings.Replace(base, `"summary":"ok"`, `"Summary":"ok"`, 1), strings.Replace(base, `"summary":"ok"`, `"summary":"bad\ud800"`, 1), strings.Replace(base, `"summary":"ok"`, `"evidence":[null]`, 1), strings.Replace(base, `"summary":"ok"`, `"summary":"ok","unexpected":1`, 1)}
	for _, raw := range cases {
		diagnosticResult(t, call(t, session, "job_update", json.RawMessage(raw)), "invalid_arguments", false)
	}
	if count.Load() != 0 {
		t.Fatal("invalid arguments reached HTTP")
	}
}

func TestUnifiedJobReadStrictBeforeCredentialsAndCancellation(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": identity}, "missing-credential"), "alpha"), "")
	for _, raw := range []string{`{}`, `null`, `{"job_id":null}`, `{"job_id":"../job"}`, `{"job_id":"job","limit":0}`, `{"job_id":"job","limit":101}`, `{"job_id":"job","limit":null}`, `{"job_id":"job","limit":1.0}`, `{"job_id":"job","limit":1,"limit":2}`, `{"job_id":"job","worker_id":"other"}`, `{"job_id":"job","cursor":"next"}`, `{"Job_id":"job"}`, `{"job_id":"\ud800"}`} {
		diagnosticResult(t, call(t, session, "job_read", json.RawMessage(raw)), "invalid_arguments", false)
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
		if !strings.HasSuffix(r.URL.Path, "/jobs/job-one") {
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
	diagnosticResult(t, call(t, bounded, "job_read", map[string]any{"job_id": "job-one"}), "deadline_exceeded", false)
	if reads.Load() != 1 || time.Since(started) > time.Second {
		t.Fatal("cancellation/retry", reads.Load())
	}
}
