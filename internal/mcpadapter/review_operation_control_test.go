package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReviewOperationControlSDKActualGuardReplayAndWait(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var lose atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/control") && lose.Swap(false) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != 200 {
				t.Error(rec.Code, rec.Body.String())
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Code)
			w.Write([]byte("{"))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	args := executionArguments()
	args["channel"] = "cli"
	var queued client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_request", args), &queued)
	before := f.providers.Load()
	control := map[string]any{"operation_id": queued.ID, "expected_revision": 1, "action": "pause", "idempotency_key": "pause"}
	lose.Store(true)
	diagnosticResult(t, call(t, session, "review_operation_control", control), "invalid_response", true)
	var first, replay client.ReviewOperationControlReceipt
	decodeResult(t, call(t, session, "review_operation_control", control), &first)
	decodeResult(t, call(t, session, "review_operation_control", control), &replay)
	if first != replay || !first.Paused || first.Revision != 2 {
		t.Fatal(first, replay)
	}
	var status client.ReviewOperationStatus
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": queued.ID}), &status)
	if status.Wait == nil || !status.Wait.Paused || status.Wait.RetryNotBefore != "" || status.AttemptID != "" || status.State != "queued" {
		t.Fatal(status)
	}
	control["action"], control["idempotency_key"] = "resume", "stale"
	diagnosticResult(t, call(t, session, "review_operation_control", control), "review_conflict", false)
	control["expected_revision"], control["idempotency_key"] = 2, "resume"
	var resumed client.ReviewOperationControlReceipt
	decodeResult(t, call(t, session, "review_operation_control", control), &resumed)
	if resumed.Paused || resumed.State != "queued" || resumed.Revision != 3 {
		t.Fatal(resumed)
	}
	control["expected_revision"], control["idempotency_key"], control["action"] = 3, "cancel", "cancel"
	var cancelled client.ReviewOperationControlReceipt
	decodeResult(t, call(t, session, "review_operation_control", control), &cancelled)
	if cancelled.State != "cancelled" {
		t.Fatal(cancelled)
	}
	if f.providers.Load() != before {
		t.Fatal("controls called provider")
	}
}

func TestReviewOperationControlSDKStrictBeforeCredentials(t *testing.T) {
	binding := localContext()
	binding.ServerURL = "http://127.0.0.1:1"
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"alpha": binding}, "missing-private-token"), "alpha")
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, raw := range []string{`{}`, `{"operation_id":"` + id + `","expected_revision":1,"action":"pause","idempotency_key":"x","worker_id":"other"}`, `{"operation_id":"` + id + `","expected_revision":null,"action":"pause","idempotency_key":"x"}`, `{"operation_id":"` + id + `","expected_revision":1,"action":"stop","idempotency_key":"x"}`, `{"operation_id":"` + id + `","expected_revision":1,"action":"pause","action":"resume","idempotency_key":"x"}`} {
		diagnosticResult(t, call(t, session, "review_operation_control", json.RawMessage(raw)), "invalid_arguments", false)
	}
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "review_operation_control" {
			found = true
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
				t.Fatal("wrong control annotations")
			}
		}
	}
	if !found {
		t.Fatal("control missing discovery")
	}
}
