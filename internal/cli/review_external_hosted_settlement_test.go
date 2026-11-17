package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestExternalHostedSettlementCLIValidatesFileAndSendsBoundMutation(t *testing.T) {
	runID := "review_run_" + strings.Repeat("a", 32)
	valid := `{"count_round":false,"runs":[{"run_id":"` + runID + `","findings":[]}]}`
	missingChoice := importCLIFile(t, []byte(strings.Replace(valid, `"count_round":false,`, "", 1)))
	code, out, diagnostic := run([]string{"--profiles-file", "/missing/config", "reviews", "settle-hosted", "--pr", "2", "--file", missingChoice, "--key", "settle-once"}, nil)
	if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "reviews settle-hosted --help") {
		t.Fatal("missing choice reached connection setup or lost help", code, diagnostic)
	}
	f := setupServiceFixture(t)
	project := f.project(t, "external-hosted-cli")
	worker := f.worker(t, project, "Builder")
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.URL.Path == "/api/v1/projects/"+project+"/reviews/requests" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "request-one",
				"error": map[string]any{"code": "external_hosted_pending", "message": "Pending external Hosted review",
					"details": map[string]any{"pull_number": 2, "run_ids": []string{runID}, "has_more": false}},
			})
			return
		}
		posts.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/"+project+"/reviews/external-hosted/2/settlement" ||
			r.Header.Get("X-FC-Worker") != worker || r.Header.Get("Idempotency-Key") != "settle-once" {
			t.Error("CLI settlement lost route or binding")
		}
		raw, err := io.ReadAll(r.Body)
		value, decodeErr := client.DecodeExternalHostedSettlementRequest(raw)
		if err != nil || decodeErr != nil || value.CountRound || len(value.Runs) != 1 || value.Runs[0].RunID != runID {
			t.Error("CLI settlement changed the file's intent", err, decodeErr)
		}
		_ = json.NewEncoder(w).Encode(client.ExternalHostedSettlementReceipt{ID: "review_hosted_batch_" + strings.Repeat("b", 32), State: "external_hosted_settled", Runs: 1})
	}))
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	code, out, diagnostic = mutationInvoke(t, profiles, "builder", "reviews", "request", "2", "--channel", "hosted", "--expect-config", "1", "--head", strings.Repeat("b", 40), "--wait-seconds", "0", "--key", "request-once")
	if code == cli.ExitSuccess || out != "" || !strings.Contains(diagnostic, "reviews settle-hosted") || !strings.Contains(diagnostic, runID) {
		t.Fatal("CLI review refusal lost settlement guidance", code, diagnostic)
	}
	code, out, diagnostic = mutationInvoke(t, profiles, "builder", "reviews", "settle-hosted", "--pr", "2", "--file", importCLIFile(t, []byte(valid)), "--key", "settle-once")
	var receipt map[string]any
	if code != cli.ExitSuccess || diagnostic != "" || json.Unmarshal([]byte(out), &receipt) != nil || len(receipt) != 4 || receipt["round_counted"] != false || posts.Load() != 1 {
		t.Fatal("CLI settlement failed to return a compact receipt", code, diagnostic, receipt, posts.Load())
	}
}
