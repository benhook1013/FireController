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

func TestReviewBatchMCPDiscoveryAndPreConnectionValidation(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	c := localContext()
	c.ServerURL = server.URL
	path := profiles(t, map[string]profile.Context{"batch": c}, "missing-private-credential")
	session := reviewControlsStdio(t, path, "batch")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range listed.Tools {
		if tool.Name != "review_findings_decide" && tool.Name != "review_findings_fix" {
			continue
		}
		found++
		a := tool.Annotations
		if a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Fatal("batch annotations changed authority", a)
		}
		encoded, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(encoded), `"maxItems":100`) || !strings.Contains(string(encoded), `"additionalProperties":false`) {
			t.Fatal("batch schema missing explicit bounded items")
		}
	}
	if found != 2 {
		t.Fatal("batch tools missing", found)
	}
	base := map[string]any{"evidence_id": "review_evidence_" + strings.Repeat("a", 32), "idempotency_key": "explicit", "expected_revision": 0, "items": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Explicit selected reason"}}}
	for _, items := range []any{nil, []any{}, []any{nil}, []any{map[string]any{"disposition": "accepted", "reason": "No selected index"}}, []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Reason", "expected_route_revision": nil}}, []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Reason"}, map[string]any{"finding": 0, "disposition": "rejected", "reason": "Reason"}}} {
		args := map[string]any{}
		for k, v := range base {
			args[k] = v
		}
		args["items"] = items
		diagnosticResult(t, call(t, session, "review_findings_decide", args), "invalid_arguments", false)
	}
	diagnosticResult(t, call(t, session, "review_findings_fix", map[string]any{"evidence_id": base["evidence_id"], "idempotency_key": "explicit", "expected_revision": 0, "items": []any{map[string]any{"finding": 0, "fix_sha": strings.Repeat("a", 40), "proof_note": "Proof"}}}), "invalid_arguments", false)
	if requests.Load() != 0 {
		t.Fatal("malformed batch reached credentials/network", requests.Load())
	}
}

func TestReviewBatchMCPActualAtomicReceiptReplayAndExplicitFinalization(t *testing.T) {
	f, handler, id := reviewControlsFixture(t)
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/batch") {
			posts.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	decision := map[string]any{"evidence_id": id, "idempotency_key": "batch-decisions", "expected_revision": 0, "items": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Explicit decision with source proof"}}}
	var receipt, replay client.ReviewControlReceipt
	decodeResult(t, call(t, session, "review_findings_decide", decision), &receipt)
	decodeResult(t, call(t, session, "review_findings_decide", decision), &replay)
	if receipt != replay || receipt.ID != id || receipt.State != "adjudicated_batch" || receipt.Revision != 1 {
		t.Fatal("batch decision replay changed", receipt, replay)
	}
	fix := map[string]any{"evidence_id": id, "idempotency_key": "batch-fixes", "expected_revision": 1, "items": []any{map[string]any{"finding": 0, "expected_sha": "", "fix_sha": strings.Repeat("a", 40), "proof_note": "Exact focused source proof"}}}
	decodeResult(t, call(t, session, "review_findings_fix", fix), &receipt)
	decodeResult(t, call(t, session, "review_findings_fix", fix), &replay)
	if receipt != replay || receipt.State != "fixes_recorded_batch" || receipt.Revision != 2 {
		t.Fatal("batch fix replay changed", receipt, replay)
	}
	var state client.ReviewAdjudication
	decodeResult(t, call(t, session, "review_adjudication_read", map[string]any{"evidence_id": id}), &state)
	if state.Finalized || state.FrozenCounts != nil || state.Findings[0].FixSHA != strings.Repeat("a", 40) {
		t.Fatal("batch fabricated finalization or lost proof", state)
	}
	encoded, _ := json.Marshal(receipt)
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	if len(fields) != 3 || len(encoded) > 200 || posts.Load() != 4 {
		t.Fatal("batch split/retried or expanded receipt", posts.Load())
	}
	decision["items"] = []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Changed original batch"}}
	diagnosticResult(t, call(t, session, "review_findings_decide", decision), "idempotency_conflict", false)
	// 100 fully escaped reasons exceed both the old per-tool envelope and the
	// former ~400KiB stdio line bound. Reach real selected-index validation in
	// the one-finding fixture; no first-item correction may survive that failure.
	items := make([]any, 100)
	for i := range items {
		items[i] = map[string]any{"finding": i, "disposition": "accepted", "reason": strings.Repeat("<", 2048)}
	}
	large := map[string]any{"evidence_id": id, "idempotency_key": "large-selected-body", "expected_revision": 2, "items": items}
	encoded, _ = json.Marshal(large)
	if len(encoded) <= 400<<10 {
		t.Fatal("fixture did not exercise escaped stdio envelope")
	}
	diagnosticResult(t, call(t, session, "review_findings_decide", large), "validation_failed", false)
	decodeResult(t, call(t, session, "review_adjudication_read", map[string]any{"evidence_id": id}), &state)
	if state.Revision != 2 || state.Findings[0].Reason != "Explicit decision with source proof" {
		t.Fatal("large bad-middle stdio batch left partial source correction", state)
	}
}
