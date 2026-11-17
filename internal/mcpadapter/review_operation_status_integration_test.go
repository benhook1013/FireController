package mcpadapter_test

import (
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
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
	for _, args := range []map[string]any{{"operation_id": receipt.ID, "wait_seconds": 901}, {"operation_id": receipt.ID, "diagnostics": nil}} {
		diagnosticResult(t, call(t, session, "review_operation_read", args), "invalid_arguments", false)
	}
}

func TestOperationStatusWaitDiagnosticBeforeCredentialsOrNetwork(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	bound := localContext()
	bound.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": bound}, "missing-private-credential"), "chosen"), "")
	id := "review_operation_" + strings.Repeat("a", 32)
	d := diagnosticResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id, "wait_seconds": 901}), "invalid_arguments", false)
	for _, phrase := range []string{"wait_seconds", "MCP", "0 to 900", "CLI", "900"} {
		if !strings.Contains(d.Message, phrase) {
			t.Fatal("missing field-specific MCP/CLI bound", d)
		}
	}
	if !strings.Contains(d.Action, "No operation request was sent") || strings.Contains(strings.ToLower(d.Action), "maintainer") || requests.Load() != 0 {
		t.Fatal("invalid wait reached credentials/network or required escalation", d, requests.Load())
	}
	// Valid bounds pass argument admission and reach the existing credential
	// refusal; they neither start a wait nor contact the synthetic service here.
	for _, args := range []map[string]any{{"operation_id": id}, {"operation_id": id, "wait_seconds": 0}, {"operation_id": id, "wait_seconds": 900}} {
		diagnosticResult(t, call(t, session, "review_operation_read", args), "credential_unreadable", false)
	}
	if requests.Load() != 0 {
		t.Fatal("credential-less tool contacted the service")
	}
}
