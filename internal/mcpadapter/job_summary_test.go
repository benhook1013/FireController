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

func TestJobSummaryMCPRoundTripLostResponseAndPrivateConservation(t *testing.T) {
	f := mutationFixture(t)
	var damage atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/summary") {
			writes.Add(1)
			if damage.CompareAndSwap(true, false) {
				recorder := httptest.NewRecorder()
				f.handler.ServeHTTP(recorder, r)
				if recorder.Code != 200 {
					t.Errorf("summary did not commit: %s", recorder.Body.String())
				}
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"id":"incomplete"}`))
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "summary-job", "title": "Private title", "brief": "Private instructions"}), &job)
	var empty client.JobShareableSummary
	decodeResult(t, call(t, session, "job_summary_read", map[string]any{"job_id": job.ID}), &empty)
	if empty.Revision != 0 || empty.Title != "" || empty.Body != "" {
		t.Fatal(empty)
	}
	set := map[string]any{"job_id": job.ID, "idempotency_key": "summary-once", "expected_job_revision": 1, "expected_revision": 0, "title": "Deliberate title", "body": "A concise **Markdown** summary."}
	damage.Store(true)
	diagnosticResult(t, call(t, session, "job_summary_set", set), "invalid_response", true)
	var receipt, replay client.JobSummaryReceipt
	decodeResult(t, call(t, session, "job_summary_set", set), &receipt)
	decodeResult(t, call(t, session, "job_summary_set", set), &replay)
	if receipt != replay || receipt.Revision != 1 || writes.Load() != 3 {
		t.Fatal("uncertain summary retried automatically or duplicate changed", receipt, replay, writes.Load())
	}
	var summary client.JobShareableSummary
	decodeResult(t, call(t, session, "job_summary_read", map[string]any{"job_id": job.ID}), &summary)
	if summary.Title != set["title"] || summary.Body != set["body"] || summary.Revision != 1 {
		t.Fatal(summary)
	}
	var private client.Job
	decodeResult(t, call(t, session, "job_read", map[string]any{"job_id": job.ID}), &private)
	if private.Revision != 1 || private.Title != "Private title" || private.Brief != "Private instructions" || f.providers.Load() != 1 {
		t.Fatal("summary changed private job/provider state")
	}
	set["idempotency_key"] = "clear"
	set["expected_revision"] = 1
	set["title"] = ""
	set["body"] = ""
	decodeResult(t, call(t, session, "job_summary_set", set), &receipt)
	if receipt.Revision != 2 {
		t.Fatal(receipt)
	}
	set["idempotency_key"] = "stale"
	set["title"] = "Title"
	set["body"] = "Body"
	diagnosticResult(t, call(t, session, "job_summary_set", set), "summary_revision_conflict", false)
}

func TestJobSummaryMCPInvalidInputsBeforeCredentialOrNetwork(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": binding}, "missing-summary-credential"), "alpha"), "")
	base := `{"job_id":"job-one","idempotency_key":"key","expected_job_revision":1,"expected_revision":0,"title":"Title","body":"Body"}`
	cases := []struct{ name, raw string }{{"job_summary_read", `{}`}, {"job_summary_read", `{"job_id":"../other"}`}, {"job_summary_read", `{"job_id":"job-one","title":"unknown"}`}}
	for _, pair := range [][2]string{{`"body":"Body"`, `"body":null`}, {`"body":"Body"`, `"body":""`}, {`"body":"Body"`, `"body":"bad\ud800"`}, {`"body":"Body"`, `"body":"Body","body":"duplicate"`}, {`"title":"Title"`, `"title":"` + strings.Repeat("雪", 81) + `"`}, {`"expected_revision":0`, `"expected_revision":true`}, {`"expected_revision":0`, `"expected_revision":0.5`}, {`"expected_revision":0`, `"expected_revision":9223372036854775807`}, {`"expected_revision":0`, `"Expected_revision":0`}, {`"expected_job_revision":1`, `"expected_job_revision":0`}, {`,"title":"Title"`, ``}} {
		cases = append(cases, struct{ name, raw string }{"job_summary_set", strings.Replace(base, pair[0], pair[1], 1)})
	}
	for _, c := range cases {
		errorResult(t, call(t, session, c.name, json.RawMessage(c.raw)), "invalid_arguments")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid summary used credentials/network", requests.Load())
	}
}
