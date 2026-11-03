package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFixGuardMCPDescribesAndRecoversStoredSHAConflict(t *testing.T) {
	f, handler, id := reviewControlsFixture(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	tools, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, tool := range tools.Tools {
		if tool.Name != "review_finding_fix" && tool.Name != "review_findings_fix" && tool.Name != "review_route_fix" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema map[string]any
		json.Unmarshal(raw, &schema)
		properties := schema["properties"].(map[string]any)
		if tool.Name == "review_findings_fix" {
			properties = properties["items"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		}
		description, _ := properties["expected_sha"].(map[string]any)["description"].(string)
		if !strings.Contains(description, "fix_sha already stored") || !strings.Contains(description, "empty for the first fix") || !strings.Contains(description, "not the PR head") {
			t.Fatal("expected_sha semantics missing from field schema", tool.Name)
		}
		checked++
	}
	if checked != 3 {
		t.Fatal("fix tool schemas missing", checked)
	}
	var receipt client.ReviewControlReceipt
	decodeResult(t, call(t, session, "review_findings_decide", map[string]any{"evidence_id": id, "idempotency_key": "accept", "expected_revision": 0, "items": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Verified source"}}}), &receipt)
	item := map[string]any{"finding": 0, "expected_sha": strings.Repeat("b", 40), "fix_sha": strings.Repeat("b", 40), "proof_note": "Verified published fix"}
	args := map[string]any{"evidence_id": id, "idempotency_key": "wrong-head-guard", "expected_revision": 1, "items": []any{item}}
	failure := diagnosticResult(t, call(t, session, "review_findings_fix", args), "review_conflict", false)
	details, _ := failure.Details.(map[string]any)
	if details["reason"] != "fix_sha_conflict" || details["finding"] != float64(0) || details["current_fix_sha"] != "" || !strings.Contains(failure.Action, "empty string") {
		t.Fatal("guard facts or direct recovery lost", failure)
	}
	var state client.ReviewAdjudication
	decodeResult(t, call(t, session, "review_adjudication_read", map[string]any{"evidence_id": id}), &state)
	if state.Revision != 1 || state.Findings[0].FixSHA != "" {
		t.Fatal("failed batch changed source")
	}
	args["idempotency_key"] = "correct-stored-value"
	item["expected_sha"] = ""
	decodeResult(t, call(t, session, "review_findings_fix", args), &receipt)
	if receipt.Revision != 2 || receipt.State != "fixes_recorded_batch" {
		t.Fatal("supported recovery failed", receipt)
	}
}
