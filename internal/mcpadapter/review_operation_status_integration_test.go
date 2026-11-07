package mcpadapter_test

import (
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOperationStatusStdioCompactDiagnosticsAndNoProviders(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var compactReads, fullReads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews/operations/") {
			if strings.HasSuffix(r.URL.Path, "/status") {
				compactReads.Add(1)
			} else {
				fullReads.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	before := f.providers.Load()
	var receipt client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_request", executionArguments()), &receipt)
	var compact client.ReviewOperationStatus
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": receipt.ID}), &compact)
	if compact.Stage != "queued" || compact.NextAction != "wait" || compact.ID != receipt.ID {
		t.Fatal(compact)
	}
	var full client.ReviewOperation
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": receipt.ID, "diagnostics": true}), &full)
	if full.ProjectID != f.project || full.WorkerID != f.sender || full.ID != receipt.ID {
		t.Fatal(full)
	}
	if compactReads.Load() != 1 || fullReads.Load() != 1 || before != f.providers.Load() {
		t.Fatal("output view polled or fetched provider")
	}
	for _, args := range []map[string]any{{"operation_id": receipt.ID, "wait_seconds": 51}, {"operation_id": receipt.ID, "diagnostics": nil}} {
		diagnosticResult(t, call(t, session, "review_operation_read", args), "invalid_arguments", false)
	}
}
