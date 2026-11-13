package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestJobStatusMCPCompactSingleRead(t *testing.T) {
	f := mutationFixture(t)
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/jobs/") {
			if !strings.HasSuffix(r.URL.Path, "/status") {
				t.Errorf("unexpected status fanout: %s", r.URL.Path)
			}
			reads.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "status-job", "title": "Current task", "brief": "Private definition"}), &job)
	var receipt client.JobReceipt
	decodeResult(t, call(t, session, "job_update", map[string]any{"job_id": job.ID, "idempotency_key": "status-progress", "expected_revision": 1, "progress": map[string]any{"summary": strings.Repeat("雪", 400), "next": "continue", "blocker": "", "evidence": []string{"private-reference"}}}), &receipt)
	var status client.JobStatus
	decodeResult(t, call(t, session, "job_status_read", map[string]any{"job_id": job.ID}), &status)
	if status.ID != job.ID || status.Revision != 2 || len(status.ProgressSummary) != 1023 || !status.ProgressTruncated || status.EvidenceCount != 1 || status.Next != "continue" || status.Blocker != "" || status.BlockerTruncated || reads.Load() != 1 || f.providers.Load() != 1 {
		t.Fatal("inaccurate/fanout status", status, reads.Load())
	}
}

func TestJobStatusMCPInvalidInputsBeforeCredentialOrNetwork(t *testing.T) {
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reads.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": binding}, "missing-status-credential"), "alpha"), "")
	for _, raw := range []string{`{}`, `null`, `{"job_id":null}`, `{"job_id":1}`, `{"job_id":"../other"}`, `{"job_id":"job-one","limit":1}`, `{"job_id":"job-one","job_id":"job-two"}`, `{"Job_id":"job-one"}`} {
		errorResult(t, call(t, session, "job_status_read", json.RawMessage(raw)), "invalid_arguments")
	}
	if reads.Load() != 0 {
		t.Fatal("invalid status input contacted service")
	}
}
