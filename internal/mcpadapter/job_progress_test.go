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

func TestJobProgressToolsRoundTripAndRecoverUncertainWrites(t *testing.T) {
	f := mutationFixture(t)
	var corrupt atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/updates") {
			writes.Add(1)
			if corrupt.CompareAndSwap(true, false) {
				recorder := httptest.NewRecorder()
				f.handler.ServeHTTP(recorder, r)
				if recorder.Code != http.StatusCreated {
					t.Error("update did not commit before response damage")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"incomplete"}`))
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "progress-setup", "title": "Progress", "brief": "Standing instructions"}), &job)
	var empty client.Checklist
	decodeResult(t, call(t, session, "job_checklist_read", map[string]any{"job_id": job.ID}), &empty)
	if empty.Revision != 0 || empty.Items == nil || empty.JobRevision != 1 {
		t.Fatal("initial checklist not explicit")
	}
	update := map[string]any{"job_id": job.ID, "idempotency_key": "progress-once", "expected_job_revision": 1, "body": "  PRIVATE_UPDATE\n", "evidence": []string{"proof:one", "proof:two"}}
	corrupt.Store(true)
	diagnosticResult(t, call(t, session, "job_update_append", update), "invalid_response", true)
	var receipt, replay client.UpdateReceipt
	decodeResult(t, call(t, session, "job_update_append", update), &receipt)
	decodeResult(t, call(t, session, "job_update_append", update), &replay)
	if receipt != replay || receipt.JobRevision != 1 || receipt.Sequence < 1 || writes.Load() != 3 {
		t.Fatal("uncertain update was automatically retried or duplicate replay changed")
	}
	var updates client.Page[client.Update]
	decodeResult(t, call(t, session, "job_update_history", map[string]any{"job_id": job.ID}), &updates)
	if len(updates.Items) != 1 || updates.Items[0].Body != "  PRIVATE_UPDATE\n" || updates.Items[0].WorkerID != f.sender || updates.HasMore {
		t.Fatal("update archive lost text/attribution or fetched unexpected history")
	}
	items := []map[string]any{{"id": "prove", "text": "Check behavior", "done": false}}
	set := map[string]any{"job_id": job.ID, "idempotency_key": "checklist-once", "expected_job_revision": 1, "expected_revision": 0, "items": items}
	var setReceipt client.ChecklistReceipt
	decodeResult(t, call(t, session, "job_checklist_replace", set), &setReceipt)
	if setReceipt.Revision != 1 || setReceipt.Outcome != "updated" {
		t.Fatal("checklist replacement receipt changed")
	}
	var state client.Checklist
	decodeResult(t, call(t, session, "job_checklist_read", map[string]any{"job_id": job.ID}), &state)
	if state.Revision != 1 || state.JobRevision != 1 || len(state.Items) != 1 || state.Items[0].Done {
		t.Fatal("read changed checklist completion")
	}
	set["idempotency_key"], set["expected_revision"] = "checklist-done", 1
	items[0]["done"] = true
	decodeResult(t, call(t, session, "job_checklist_replace", set), &setReceipt)
	var history client.Page[client.ChecklistHistoryEntry]
	decodeResult(t, call(t, session, "job_checklist_history", map[string]any{"job_id": job.ID, "limit": 1}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 2 || !history.Items[0].Items[0].Done || !history.HasMore {
		t.Fatal("checklist current history lost completion")
	}
	decodeResult(t, call(t, session, "job_checklist_history", map[string]any{"job_id": job.ID, "limit": 1, "cursor": history.Cursor}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 1 || history.Items[0].Items[0].Done || history.HasMore {
		t.Fatal("checklist older history was rewritten")
	}
	var current client.Job
	decodeResult(t, call(t, session, "job_read", map[string]any{"job_id": job.ID}), &current)
	if current.Revision != 1 || current.Brief != "Standing instructions" || f.providers.Load() != 1 {
		t.Fatal("progress changed brief/revision or performed provider calls")
	}
}

func TestJobProgressToolsRejectBeforeCredentialAndNetwork(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, "missing-private-credential")
	session := connect(t, adapter(t, path, "alpha"), "")
	base := `{"job_id":"job-one","idempotency_key":"explicit","expected_job_revision":1,"expected_revision":0,"items":[{"id":"prove","text":"Proof","done":false}]}`
	cases := []struct{ name, raw string }{
		{"job_checklist_replace", strings.Replace(base, `,"done":false`, "", 1)},
		{"job_checklist_replace", strings.Replace(base, `"done":false`, `"done":null`, 1)},
		{"job_checklist_replace", strings.Replace(base, `"done":false`, `"done":false,"done":true`, 1)},
		{"job_checklist_replace", strings.Replace(base, `"done":false`, `"Done":false`, 1)},
		{"job_checklist_replace", strings.Replace(base, `"id":"prove"`, `"id":"bad/id"`, 1)},
		{"job_checklist_replace", strings.Replace(base, `"expected_revision":0`, `"expected_revision":null`, 1)},
		{"job_checklist_replace", strings.Replace(base, `"expected_revision":0,`, "", 1)},
		{"job_checklist_replace", strings.Replace(base, `"expected_revision":0`, `"expected_revision":9223372036854775807`, 1)},
		{"job_checklist_replace", strings.Replace(base, `"items":[{"id":"prove","text":"Proof","done":false}]`, `"items":null`, 1)},
		{"job_update_append", `{"job_id":"job-one","idempotency_key":"explicit","expected_job_revision":1,"body":"Update","evidence":null}`},
		{"job_update_append", `{"job_id":"job-one","idempotency_key":"explicit","expected_job_revision":1,"body":"Update","evidence":[null]}`},
		{"job_update_append", `{"job_id":"job-one","idempotency_key":"explicit","expected_job_revision":1,"body":"bad\ud800"}`},
		{"job_update_append", `{"job_id":"job-one","idempotency_key":"explicit","expected_job_revision":1,"body":"Update","actor_id":"override"}`},
		{"job_checklist_read", `{"job_id":"job-one","limit":1}`},
		{"job_checklist_history", `{"job_id":"job-one","limit":0}`},
		{"job_checklist_history", `{"job_id":"job-one","cursor":""}`},
		{"job_update_history", `{"job_id":"job-one","cursor":""}`},
		{"job_update_history", `{"job_id":"../other"}`},
	}
	for _, test := range cases {
		diagnosticResult(t, call(t, session, test.name, json.RawMessage(test.raw)), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid tools made %d HTTP requests", requests.Load())
	}
}
