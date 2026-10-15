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

func TestCheckpointToolsReceiptsLatestArchiveAndUnknownOutcome(t *testing.T) {
	f := mutationFixture(t)
	var corrupt atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/checkpoints") {
			writes.Add(1)
			if corrupt.CompareAndSwap(true, false) {
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, r)
				if rec.Code != 201 {
					t.Error("checkpoint not committed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":"incomplete"}`))
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "job-setup", "title": "Checkpoint job", "brief": "Original brief"}), &job)
	var latest client.LatestCheckpointResult
	decodeResult(t, call(t, session, "job_checkpoint_latest", map[string]any{"job_id": job.ID}), &latest)
	if latest.Checkpoint != nil || latest.JobRevision != 1 {
		t.Fatal("missing checkpoint not explicit null")
	}
	request := map[string]any{"job_id": job.ID, "idempotency_key": "checkpoint-once", "expected_job_revision": 1, "done": "  PRIVATE_CHECKPOINT\n", "next": "next\tstep", "evidence": []string{"proof-two", "proof-one"}}
	var receipt, replay client.CheckpointReceipt
	decodeResult(t, call(t, session, "job_checkpoint_append", request), &receipt)
	decodeResult(t, call(t, session, "job_checkpoint_append", request), &replay)
	if receipt != replay || receipt.Sequence < 1 || receipt.JobRevision != 1 || receipt.Outcome != "created" {
		t.Fatal("checkpoint compact receipt/replay changed")
	}
	compact, _ := json.Marshal(receipt)
	if strings.Contains(string(compact), "PRIVATE_CHECKPOINT") || strings.Contains(string(compact), "evidence") {
		t.Fatal("receipt echoes checkpoint text")
	}
	decodeResult(t, call(t, session, "job_checkpoint_latest", map[string]any{"job_id": job.ID}), &latest)
	if latest.Checkpoint == nil || latest.Checkpoint.Done != "  PRIVATE_CHECKPOINT\n" || latest.Checkpoint.WorkerID != f.sender || strings.Join(latest.Checkpoint.Evidence, ",") != "proof-two,proof-one" {
		t.Fatal("latest checkpoint text/order/identity changed")
	}
	corrupt.Store(true)
	uncertain := map[string]any{"job_id": job.ID, "idempotency_key": "checkpoint-uncertain", "expected_job_revision": 1, "done": "second", "next": "continue"}
	diagnosticResult(t, call(t, session, "job_checkpoint_append", uncertain), "invalid_response", true)
	decodeResult(t, call(t, session, "job_checkpoint_append", uncertain), &replay)
	if writes.Load() != 4 || replay.ID == receipt.ID {
		t.Fatal("automatic retry or lost second receipt")
	}
	var first, second client.Page[client.Checkpoint]
	decodeResult(t, call(t, session, "job_checkpoint_history", map[string]any{"job_id": job.ID, "limit": 1}), &first)
	if len(first.Items) != 1 || !first.HasMore || first.Cursor == "" {
		t.Fatal("checkpoint archive first page incomplete")
	}
	decodeResult(t, call(t, session, "job_checkpoint_history", map[string]any{"job_id": job.ID, "limit": 1, "cursor": first.Cursor}), &second)
	if len(second.Items) != 1 || second.HasMore || first.Items[0].Sequence <= second.Items[0].Sequence || second.Items[0].ID != receipt.ID {
		t.Fatal("checkpoint archive ordering/completeness changed")
	}
	if f.providers.Load() != 1 {
		t.Fatal("checkpoint tool performed provider work")
	}
}

func TestCheckpointToolArgumentsRejectBeforeCredentialAndNetwork(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"alpha": c}, "absent-private-credential")
	session := connect(t, adapter(t, path, "alpha"), "")
	base := `{"job_id":"job-one","idempotency_key":"explicit","expected_job_revision":1,"done":"done","next":"next"}`
	cases := []struct{ name, raw string }{
		{"job_checkpoint_append", `{}`},
		{"job_checkpoint_append", strings.Replace(base, `"done":"done"`, `"done":null`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"done":"done"`, `"Done":"done"`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"done":"done"`, `"done":"done","done":"duplicate"`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"done":"done"`, `"done":"bad\ud800"`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"done":"done"`, `"done":"bad\u0000"`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"done":"done"`, `"done":"`+strings.Repeat("x", 8193)+`"`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"next":"next"`, `"next":"next","evidence":null`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"next":"next"`, `"next":"next","evidence":[null]`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"next":"next"`, `"next":"next","evidence":[" "]`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"next":"next"`, `"next":"next","evidence":[`+strings.Repeat(`"x",`, 10)+`"x"]`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"next":"next"`, `"next":"next","actor_id":"override"`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"expected_job_revision":1`, `"expected_job_revision":0`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"expected_job_revision":1`, `"expected_job_revision":1.5`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"expected_job_revision":1`, `"expected_job_revision":9223372036854775808`, 1)},
		{"job_checkpoint_append", strings.Replace(base, `"job-one"`, `"../other"`, 1)},
		{"job_checkpoint_latest", `{"job_id":"job-one","limit":1}`},
		{"job_checkpoint_history", `{"job_id":"job-one","limit":0}`},
		{"job_checkpoint_history", `{"job_id":"job-one","cursor":null}`},
		{"job_checkpoint_history", `{"job_id":"job-one","limit":1,"limit":2}`},
	}
	for i, test := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			diagnosticResult(t, call(t, session, test.name, json.RawMessage(test.raw)), "invalid_arguments", false)
		})
	}
	if requests.Load() != 0 {
		t.Fatal("invalid checkpoint arguments opened a credential or connection")
	}
}
