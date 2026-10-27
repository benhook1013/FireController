package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestQueueChangeMCPOrdinaryWriterAndLostResponse(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var drop atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && drop.Swap(false) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != 201 {
				t.Error(rec.Code, rec.Body.String())
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	input := map[string]any{"expected_configuration_revision": 1, "pull_number": 3, "before": 2, "idempotency_key": "mcp-add"}
	drop.Store(true)
	diagnosticResult(t, call(t, session, "review_queue_add", input), "connection_failed", true)
	var first client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_queue_add", input), &first)
	if first.ID != f.project || first.Revision != 2 {
		t.Fatal(first)
	}
	var replay client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_queue_add", input), &replay)
	if replay != first {
		t.Fatal(replay)
	}
	var moved client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_queue_move", map[string]any{"expected_configuration_revision": 2, "pull_number": 3, "position": "last", "idempotency_key": "mcp-move"}), &moved)
	if moved.Revision != 3 {
		t.Fatal(moved)
	}
	var config client.ReviewExecutionConfiguration
	decodeResult(t, call(t, session, "review_execution_read", nil), &config)
	if len(config.Queue) != 2 || config.Queue[0] != 2 || config.Queue[1] != 3 {
		t.Fatal(config)
	}
}
func TestQueueChangeMCPInvalidArgumentsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-queue-token"), "chosen")
	for _, test := range []struct{ name, raw string }{
		{"review_queue_add", `{}`},
		{"review_queue_add", `{"expected_configuration_revision":1,"pull_number":3,"before":null,"idempotency_key":"k"}`},
		{"review_queue_add", `{"expected_configuration_revision":1,"pull_number":3,"paused":true,"idempotency_key":"k"}`},
		{"review_queue_move", `{"expected_configuration_revision":1,"pull_number":3,"idempotency_key":"k"}`},
		{"review_queue_move", `{"expected_configuration_revision":1,"pull_number":3,"before":2,"position":"first","idempotency_key":"k"}`},
	} {
		diagnosticResult(t, call(t, session, test.name, json.RawMessage(test.raw)), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid queue input contacted service")
	}
}
