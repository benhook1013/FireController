package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewFixCorrectionCLIDriverGuardsReplayAndCompactReceipt(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "correction-cli")
	worker := f.worker(t, project, "Builder")
	var posts atomic.Int64
	var badReceipt atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		posts.Add(1)
		var request map[string]json.RawMessage
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/"+project+"/reviews/evidence/"+cliEvidenceID+"/corrections" || r.URL.RawQuery != "" || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.admin || r.Header.Get("Idempotency-Key") != "original-key" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request) != 4 || string(request["finding"]) != "0" || string(request["expected_revision"]) != "7" || string(request["expected_sha"]) != `"`+strings.Repeat("f", 40)+`"` || string(request["reason"]) != `"  PRIVATE_CORRECTION_REASON\n"` {
			t.Error("CLI correction changed scope, key, guards or exact text")
		}
		revision := int64(8)
		if badReceipt.Load() {
			revision = 9
		}
		_ = json.NewEncoder(w).Encode(client.ReviewControlReceipt{ID: cliEvidenceID, State: "adjudication_corrected", Revision: revision})
	}))
	defer server.Close()
	path := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	args := []string{"reviews", "correct-fix", cliEvidenceID, "--finding", "0", "--expect-revision", "7", "--expect-sha", strings.Repeat("f", 40), "--reason", "  PRIVATE_CORRECTION_REASON\n", "--key", "original-key"}
	for range 2 {
		receipt := mutationSuccess(t, path, "builder", args...)
		if len(receipt) != 3 || receipt["id"] != cliEvidenceID || receipt["state"] != "adjudication_corrected" || receipt["revision"] != float64(8) {
			t.Fatal("CLI expanded or changed the correction receipt", receipt)
		}
	}
	badReceipt.Store(true)
	code, out, stderr := mutationInvoke(t, path, "builder", args...)
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || strings.Contains(stderr, "PRIVATE_CORRECTION_REASON") || posts.Load() != 3 {
		t.Fatal("invalid correction receipt retried or lost safe uncertainty", code, out, stderr)
	}
}

func TestReviewFixCorrectionCLIHelpAndInvalidArgumentsAreLocal(t *testing.T) {
	args := []string{"reviews", "correct-fix", cliEvidenceID, "--finding", "0", "--expect-revision", "7", "--expect-sha", strings.Repeat("f", 40), "--reason", "PRIVATE_REASON", "--key", "original-key"}
	invalid := [][]string{}
	for i := 3; i < len(args); i += 2 {
		missing := append([]string(nil), args[:i]...)
		invalid = append(invalid, append(missing, args[i+2:]...))
		for _, value := range []string{"", "bad\x00"} {
			bad := append([]string(nil), args...)
			bad[i+1] = value
			invalid = append(invalid, bad)
		}
	}
	for _, pair := range [][2]string{{"--finding", "00"}, {"--finding", "100"}, {"--expect-revision", "-1"}, {"--expect-revision", "9223372036854775807"}, {"--expect-sha", "abcdef1"}, {"--expect-sha", strings.Repeat("A", 40)}, {"--reason", strings.Repeat("é", 1025)}} {
		bad := append([]string(nil), args...)
		for i := 3; i < len(bad); i += 2 {
			if bad[i] == pair[0] {
				bad[i+1] = pair[1]
			}
		}
		invalid = append(invalid, bad)
	}
	for _, extra := range [][]string{{"--finding", "0"}, {"--fix-sha", strings.Repeat("a", 40)}, {"--project", "other"}, {"--expect-route-revision", "1"}} {
		invalid = append(invalid, append(append([]string(nil), args...), extra...))
	}
	for _, bad := range invalid {
		code, out, stderr := run(append([]string{"--profiles-file", "/unavailable/correction-profiles"}, bad...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") || strings.Contains(stderr, "PRIVATE_REASON") {
			t.Fatal("invalid correction reached profiles or exposed private reason", code, out, stderr)
		}
	}
	for _, help := range [][]string{{"reviews", "correct-fix", "--help"}, {"reviews", "help"}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/unavailable/correction-profiles"}, help...), map[string]string{"FC_PROFILE": "missing"})
		if code != 0 || stderr != "" || !strings.Contains(out, "correct-fix") {
			t.Fatal("correction help/discovery is not local", code, out, stderr)
		}
		if len(help) == 3 {
			for _, phrase := range []string{"--expect-sha FULL_SHA", "zero-based", "finalized", "private audit", "frozen counts", "same key, exact request"} {
				if !strings.Contains(out, phrase) {
					t.Fatal("correction help omitted guard or recovery semantics", phrase)
				}
			}
		}
	}
}
