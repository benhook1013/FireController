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
	restore := map[string]any{"expected_configuration_revision": 5, "pull_number": 3, "idempotency_key": "mcp-restore-3"}
	drop.Store(true)
	diagnosticResult(t, call(t, session, "review_queue_restore", restore), "connection_failed", true)
	var restored client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_queue_restore", restore), &restored)
	if restored.Revision != 6 || restored.State != "configured" {
		t.Fatal(restored)
	}
	decodeResult(t, call(t, session, "review_queue_restore", map[string]any{"expected_configuration_revision": 6, "pull_number": 2, "position": "first", "idempotency_key": "mcp-restore-2"}), &final)
	if final.Revision != 7 {
		t.Fatal(final)
	}
	reorder := map[string]any{"expected_configuration_revision": 7, "order": []int64{3, 2}, "idempotency_key": "mcp-reorder"}
	drop.Store(true)
	diagnosticResult(t, call(t, session, "review_queue_reorder", reorder), "connection_failed", true)
	decodeResult(t, call(t, session, "review_queue_reorder", reorder), &final)
	if final.Revision != 8 || final.State != "configured" {
		t.Fatal(final)
	}
	decodeResult(t, call(t, session, "review_queue_restore", restore), &replay)
	if replay != restored {
		t.Fatal("restore replay changed", replay, restored)
	}
	decodeResult(t, call(t, session, "review_queue_reorder", map[string]any{"expected_configuration_revision": 8, "order": []int64{3, 2}, "idempotency_key": "mcp-reorder-noop"}), &final)
	if final.Revision != 8 || final.State != "unchanged" {
		t.Fatal(final)
	}
	decodeResult(t, call(t, session, "review_execution_read", nil), &config)
	if len(config.Queue) != 2 || config.Queue[0] != 3 || config.Queue[1] != 2 {
		t.Fatal("restore/reorder dispatch did not publish selected order", config)
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
		{"review_queue_restore", `{}`},
		{"review_queue_restore", `{"expected_configuration_revision":1,"pull_number":2147483648,"idempotency_key":"k"}`},
		{"review_queue_restore", `{"expected_configuration_revision":1,"pull_number":3,"before":3,"idempotency_key":"k"}`},
		{"review_queue_restore", `{"expected_configuration_revision":1,"pull_number":3,"before":2,"position":"first","idempotency_key":"k"}`},
		{"review_queue_restore", `{"expected_configuration_revision":1,"pull_number":3,"reason":"","idempotency_key":"k"}`},
		{"review_queue_restore", `{"expected_configuration_revision":1,"pull_number":3,"order":[],"idempotency_key":"k"}`},
		{"review_queue_restore", `{"expected_configuration_revision":1,"pull_number":3,"before":null,"idempotency_key":"k"}`},
		{"review_queue_restore", `{"expected_configuration_revision":1,"pull_number":3,"idempotency_key":"k","idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":null,"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[],"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3],"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,3],"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,0],"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,-1],"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,2147483648],"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,2.5],"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,2],"pull_number":0,"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,2],"position":"","idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,2],"before":1,"idempotency_key":"k"}`},
		{"review_queue_reorder", `{"expected_configuration_revision":1,"order":[3,2],"reason":"","idempotency_key":"k"}`},
		{"review_queue_add", `{"expected_configuration_revision":1,"pull_number":3,"order":[],"idempotency_key":"k"}`},
		{"review_queue_move", `{"expected_configuration_revision":1,"pull_number":3,"position":"first","order":[],"idempotency_key":"k"}`},
		{"review_queue_remove", `{"expected_configuration_revision":1,"pull_number":3,"reason":"defunct","order":[],"idempotency_key":"k"}`},
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

func TestQueueRestoreReorderMCPExactSchemasAndAnnotations(t *testing.T) {
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": localContext()}, "missing-queue-schema-token"), "chosen")
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range tools.Tools {
		if tool.Name != "review_queue_restore" && tool.Name != "review_queue_reorder" {
			continue
		}
		found++
		var schema struct {
			AdditionalProperties bool                       `json:"additionalProperties"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
		}
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil || json.Unmarshal(encoded, &schema) != nil || schema.AdditionalProperties {
			t.Fatal(tool.Name, string(encoded), err)
		}
		allowed := []string{"expected_configuration_revision", "idempotency_key", "pull_number", "before", "after", "position"}
		required := []string{"expected_configuration_revision", "idempotency_key", "pull_number"}
		if tool.Name == "review_queue_reorder" {
			allowed = []string{"expected_configuration_revision", "idempotency_key", "order"}
			required = allowed
			var order struct {
				Type        string `json:"type"`
				MinItems    int    `json:"minItems"`
				MaxItems    int    `json:"maxItems"`
				UniqueItems bool   `json:"uniqueItems"`
				Items       struct {
					Type    string `json:"type"`
					Minimum int64  `json:"minimum"`
					Maximum int64  `json:"maximum"`
				} `json:"items"`
			}
			if json.Unmarshal(schema.Properties["order"], &order) != nil || order.Type != "array" || order.MinItems != 2 || order.MaxItems != 100 || !order.UniqueItems || order.Items.Type != "integer" || order.Items.Minimum != 1 || order.Items.Maximum != 2147483647 {
				t.Fatal("order schema differs from permitted values", string(encoded))
			}
		}
		if len(schema.Properties) != len(allowed) || len(schema.Required) != len(required) {
			t.Fatal("schema field inventory drift", string(encoded))
		}
		for _, field := range allowed {
			if _, ok := schema.Properties[field]; !ok {
				t.Fatal("missing allowed property", tool.Name, field)
			}
		}
		for _, field := range required {
			present := false
			for _, actual := range schema.Required {
				present = present || actual == field
			}
			if !present {
				t.Fatal("missing required property", tool.Name, field)
			}
		}
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || *a.OpenWorldHint || a.DestructiveHint == nil || *a.DestructiveHint != (tool.Name == "review_queue_reorder") {
			t.Fatal("queue write annotation drift", tool.Name, a)
		}
	}
	if found != 2 {
		t.Fatal("new queue tools missing", found)
	}
}
