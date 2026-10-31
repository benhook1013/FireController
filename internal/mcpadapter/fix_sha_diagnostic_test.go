package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestFixSHADiagnosticNamesSelectedFieldBeforeCredentialsOrRequests(t *testing.T) {
	var requests atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer s.Close()
	binding := localContext()
	binding.ServerURL = s.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-credential"), "chosen")
	for _, tool := range []string{"review_finding_fix", "review_route_fix", "review_findings_fix"} {
		for _, field := range []string{"fix_sha", "expected_sha"} {
			for _, bad := range []string{"0b368b8bc9", strings.Repeat("A", 40), strings.Repeat("g", 40)} {
				item := map[string]any{"finding": 0, "expected_sha": "", "fix_sha": strings.Repeat("a", 40), "proof_note": "PRIVATE PROOF BODY"}
				item[field] = bad
				args := map[string]any{"idempotency_key": "explicit-fix", "expected_revision": 1, "evidence_id": "review_evidence_" + strings.Repeat("b", 32)}
				want := field
				if tool == "review_findings_fix" {
					first := map[string]any{"finding": 1, "expected_sha": "", "fix_sha": strings.Repeat("a", 40), "proof_note": "First valid item"}
					args["items"] = []any{first, item}
					want = "items[1]." + field
				} else {
					for k, v := range item {
						args[k] = v
					}
					if tool == "review_route_fix" {
						delete(args, "evidence_id")
						delete(args, "finding")
						args["route_id"] = "review_route_" + strings.Repeat("c", 32)
					}
				}
				d := diagnosticResult(t, call(t, session, tool, args), "invalid_arguments", false)
				detail, ok := d.Details.(map[string]any)
				if !ok || detail["field"] != want || !strings.Contains(d.Message, want) || !strings.Contains(d.Message, "40 lowercase hexadecimal") || !strings.Contains(d.Action, "No request was sent") || strings.Contains(d.Action, "binding") {
					t.Fatal("malformed fix field received unrelated advice", tool, d)
				}
				body, _ := json.Marshal(d)
				if strings.Contains(string(body), bad) || strings.Contains(string(body), "PRIVATE PROOF BODY") {
					t.Fatal("diagnostic echoed private input")
				}
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid fix input contacted service")
	}
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "review_finding_fix" || tool.Name == "review_route_fix" || tool.Name == "review_findings_fix" {
			body, _ := json.Marshal(tool.InputSchema)
			if !strings.Contains(string(body), "Full 40-character lowercase hexadecimal fix commit SHA") {
				t.Fatal("discovery omitted the human-readable fix SHA constraint", tool.Name)
			}
		}
	}
}
