package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestChecklistItemCLIAndMCPShareSmallMutationAndReplay(t *testing.T) {
	f := mutationFixture(t)
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" && strings.Contains(r.URL.Path, "/checklist/items/") {
			writes.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	binding := localContext()
	binding.ServerURL, binding.InstanceID, binding.ProjectID, binding.WorkerID = service.URL, f.store.InstanceID(), f.project, f.sender
	path := profiles(t, map[string]profile.Context{"chosen": binding}, f.credential)
	session := connect(t, adapter(t, path, "chosen"), "")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "setup", "title": "Job", "brief": "Standing brief"}), &job)
	items := []client.ChecklistItem{{ID: "done", Text: "PRIVATE completed text", Done: true}, {ID: "next", Text: "  Preserve 雪\n", Done: false}}
	var plan client.ChecklistReceipt
	decodeResult(t, call(t, session, "job_checklist_replace", map[string]any{"job_id": job.ID, "idempotency_key": "plan", "expected_job_revision": 1, "expected_revision": 0, "items": items}), &plan)
	var changed client.JobReceipt
	decodeResult(t, call(t, session, "job_update", map[string]any{"job_id": job.ID, "idempotency_key": "progress", "expected_revision": 1, "progress": map[string]any{"summary": "Independent progress"}}), &changed)
	var out, stderr bytes.Buffer
	code := cli.Run([]string{"--profiles-file", path, "--profile", "chosen", "jobs", "checklist-item", job.ID, "next", "--done", "true", "--expect-checklist-revision", "1", "--key", "selected"}, nil, &out, &stderr)
	var completed client.ChecklistReceipt
	if code != cli.ExitSuccess || stderr.Len() != 0 || json.Unmarshal(out.Bytes(), &completed) != nil || completed.Revision != 2 || completed.JobRevision != 2 || strings.Contains(out.String(), "PRIVATE") {
		t.Fatal("small CLI write failed", code, out.String(), stderr.String())
	}
	var replay client.ChecklistReceipt
	decodeResult(t, call(t, session, "job_checklist_item_update", map[string]any{"job_id": job.ID, "item_id": "next", "idempotency_key": "selected", "expected_revision": 1, "done": true}), &replay)
	if replay != completed || writes.Load() != 2 {
		t.Fatal("adapters did not replay canonical same-key request", completed, replay, writes.Load())
	}
	var reopened client.ChecklistReceipt
	decodeResult(t, call(t, session, "job_checklist_item_update", map[string]any{"job_id": job.ID, "item_id": "next", "idempotency_key": "reopen", "expected_revision": 2, "done": false}), &reopened)
	var current client.Checklist
	decodeResult(t, call(t, session, "job_checklist_read", map[string]any{"job_id": job.ID}), &current)
	if current.Revision != 3 || current.JobRevision != 2 || !reflect.DeepEqual(items, current.Items) {
		t.Fatal("reopen did not preserve full ordered checklist", current)
	}
	var history client.Page[client.ChecklistHistoryEntry]
	decodeResult(t, call(t, session, "job_checklist_history", map[string]any{"job_id": job.ID, "limit": 100}), &history)
	if len(history.Items) != 3 || f.providers.Load() != 1 {
		t.Fatal("replay duplicated history or provider work occurred", len(history.Items), f.providers.Load())
	}
}

func TestChecklistItemAdaptersRejectMalformedBeforeCredentials(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"chosen": binding}, "missing-credential")
	session := connect(t, adapter(t, path, "chosen"), "")
	for _, raw := range []string{`{}`, `{"job_id":"job","item_id":"item","idempotency_key":"key","expected_revision":1}`, `{"job_id":"job","item_id":"item","idempotency_key":"key","expected_revision":0,"done":false}`, `{"job_id":"job","item_id":"item","idempotency_key":"key","expected_revision":9223372036854775807,"done":false}`, `{"job_id":"job","item_id":"bad item","idempotency_key":"key","expected_revision":1,"done":false}`, `{"job_id":"job","item_id":"item","idempotency_key":"key","expected_revision":1,"done":null}`, `{"job_id":"job","item_id":"item","idempotency_key":"key","expected_revision":1,"done":true,"done":false}`, `{"job_id":"job","item_id":"item","idempotency_key":"key","expected_revision":1,"done":true,"expected_job_revision":1}`} {
		diagnosticResult(t, call(t, session, "job_checklist_item_update", json.RawMessage(raw)), "invalid_arguments", false)
	}
	for _, tail := range [][]string{{}, {"--done", "TRUE", "--expect-checklist-revision", "1", "--key", "key"}, {"--done", "true", "--expect-checklist-revision", "0", "--key", "key"}, {"--done", "true", "--done", "false", "--expect-checklist-revision", "1", "--key", "key"}, {"--done", "true", "--expect-checklist-revision", "1", "--expect-revision", "1", "--key", "key"}} {
		args := append([]string{"--profiles-file", path, "--profile", "chosen", "jobs", "checklist-item", "job", "item"}, tail...)
		var out, stderr bytes.Buffer
		if code := cli.Run(args, nil, &out, &stderr); code != cli.ExitUsage || out.Len() != 0 || !strings.Contains(stderr.String(), "invalid_arguments") {
			t.Fatal("invalid CLI request reached credentials", code, stderr.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("malformed arguments reached network")
	}
}
