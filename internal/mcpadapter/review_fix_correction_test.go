package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewFixCorrectionMCPDriverWireReplayAndUnknownReceipt(t *testing.T) {
	f := mutationFixture(t)
	id := "review_evidence_" + strings.Repeat("a", 32)
	var posts atomic.Int64
	var invalidReceipt atomic.Bool
	invalidReceipt.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			f.handler.ServeHTTP(w, r)
			return
		}
		posts.Add(1)
		var wire map[string]json.RawMessage
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/"+f.project+"/reviews/evidence/"+id+"/corrections" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("Idempotency-Key") != "original-correction-key" || json.NewDecoder(r.Body).Decode(&wire) != nil || len(wire) != 4 || string(wire["expected_revision"]) != "7" || string(wire["finding"]) != "0" || string(wire["expected_sha"]) != `"`+strings.Repeat("f", 40)+`"` || string(wire["reason"]) != `"  PRIVATE_CORRECTION_REASON\n"` {
			t.Error("MCP correction changed exact body, guards, key or binding")
		}
		w.Header().Set("Content-Type", "application/json")
		revision := int64(8)
		if invalidReceipt.Load() {
			revision = 9
		}
		_ = json.NewEncoder(w).Encode(client.ReviewControlReceipt{ID: id, State: "adjudication_corrected", Revision: revision})
	}))
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	args := map[string]any{"evidence_id": id, "idempotency_key": "original-correction-key", "expected_revision": 7, "finding": 0, "expected_sha": strings.Repeat("f", 40), "reason": "  PRIVATE_CORRECTION_REASON\n"}
	args["expected_sha"] = ""
	diagnosticResult(t, call(t, session, "review_finding_correct_fix", args), "invalid_arguments", false)
	if posts.Load() != 0 {
		t.Fatal("empty correction SHA reached HTTP")
	}
	args["expected_sha"] = strings.Repeat("f", 40)
	failure := diagnosticResult(t, call(t, session, "review_finding_correct_fix", args), "invalid_response", true)
	if posts.Load() != 1 || !strings.Contains(failure.Action, "same idempotency key") || strings.Contains(failure.Message+failure.Action, "PRIVATE_CORRECTION_REASON") {
		t.Fatal("invalid correction receipt retried or lost private recovery guidance")
	}
	invalidReceipt.Store(false)
	for range 2 {
		result := call(t, session, "review_finding_correct_fix", args)
		var receipt client.ReviewControlReceipt
		decodeResult(t, result, &receipt)
		raw, _ := json.Marshal(result.StructuredContent)
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != 3 || receipt != (client.ReviewControlReceipt{ID: id, State: "adjudication_corrected", Revision: 8}) || strings.Contains(string(raw), "PRIVATE") {
			t.Fatal("correction replay changed revision/key or expanded private output", string(raw))
		}
	}
	if posts.Load() != 3 {
		t.Fatal("correction added unrequested mutations")
	}
}
