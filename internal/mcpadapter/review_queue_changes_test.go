package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueChangeMCPOrdinaryWriterAndLostResponse(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t, true)
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	reviews.Register(mux, f.store)
	handler = mux
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
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "review_queue_remove" && (!strings.Contains(tool.Description, "reason=superseded") || !strings.Contains(tool.Description, "Defunct merged PRs") || !strings.Contains(tool.Description, "preserving all history")) {
			t.Fatal("removal description omitted deliberate merged-carrier boundary", tool.Description)
		}
	}
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
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 1, Source: "synthetic merged carrier", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{{Number: 2, Title: "Original PR", State: "open", CIState: "unknown", Mergeability: "unknown"}, {Number: 3, Title: "Absorbed carrier", State: "merged", CIState: "unknown", Mergeability: "unknown"}}})
	seed := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/reviews/observations", bytes.NewReader(body))
	seed.Header.Set("Authorization", "Bearer "+f.admin)
	seed.Header.Set("Idempotency-Key", "mcp-fixture-merged-carrier")
	seeded := httptest.NewRecorder()
	handler.ServeHTTP(seeded, seed)
	if seeded.Code != http.StatusCreated {
		t.Fatal("merged fixture observation failed", seeded.Code, seeded.Body.String())
	}
	var historical client.ReviewPull
	decodeResult(t, call(t, session, "review_pull", map[string]any{"pull_number": 3}), &historical)
	if historical.Pull.State != "merged" {
		t.Fatal("fixture carrier was not merged", historical)
	}
	refused := diagnosticResult(t, call(t, session, "review_queue_remove", map[string]any{"expected_configuration_revision": 3, "pull_number": 3, "reason": "defunct", "idempotency_key": "mcp-merged-defunct"}), "review_queue_merged", false)
	if refused.RequestID == "" || !strings.Contains(refused.Action, "reason=superseded") || !strings.Contains(refused.Action, "preserves all history") || !strings.Contains(refused.Action, "No queue change was made") {
		t.Fatal("merged refusal lost its supported deliberate recovery", refused)
	}
	remove := map[string]any{"expected_configuration_revision": 3, "pull_number": 3, "reason": "superseded", "idempotency_key": "mcp-remove"}
	drop.Store(true)
	diagnosticResult(t, call(t, session, "review_queue_remove", remove), "connection_failed", true)
	var removed client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_queue_remove", remove), &removed)
	if removed.Revision != 4 {
		t.Fatal(removed)
	}
	var retained client.ReviewPull
	decodeResult(t, call(t, session, "review_pull", map[string]any{"pull_number": 3, "snapshot": historical.SnapshotID}), &retained)
	if retained.SnapshotID != historical.SnapshotID || retained.Pull.State != "merged" || retained.Pull.Number != 3 || retained.Pull.Title != historical.Pull.Title {
		t.Fatal("superseded carrier lost retained history", retained)
	}
	var final client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_queue_remove", map[string]any{"expected_configuration_revision": 4, "pull_number": 2, "reason": "defunct", "idempotency_key": "mcp-remove-final"}), &final)
	decodeResult(t, call(t, session, "review_execution_read", nil), &config)
	if final.Revision != 5 || config.Queue == nil || len(config.Queue) != 0 {
		t.Fatal(final, config)
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
		{"review_queue_remove", `{"expected_configuration_revision":1,"pull_number":3,"idempotency_key":"k"}`},
		{"review_queue_remove", `{"expected_configuration_revision":1,"pull_number":3,"reason":"defunct","before":2,"idempotency_key":"k"}`},
		{"review_queue_remove", `{"expected_configuration_revision":1,"pull_number":3,"reason":null,"idempotency_key":"k"}`},
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
