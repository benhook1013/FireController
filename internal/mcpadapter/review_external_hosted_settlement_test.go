package mcpadapter_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestExternalHostedSettlementMCPBoundWriteAndValidation(t *testing.T) {
	binding := localContext()
	runID := "review_run_" + strings.Repeat("a", 32)
	var requests, posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.URL.Path == "/api/v1/projects/"+binding.ProjectID+"/reviews/requests" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"request_id": "request-one",
				"error": map[string]any{"code": "external_hosted_pending", "message": "Pending external Hosted review",
					"details": map[string]any{"pull_number": 2, "run_ids": []string{runID}, "has_more": false}},
			})
			return
		}
		posts.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/"+binding.ProjectID+"/reviews/external-hosted/2/settlement" ||
			r.Header.Get("X-FC-Worker") != binding.WorkerID || r.Header.Get("Idempotency-Key") != "settle-once" ||
			r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("MCP settlement lost route or binding")
		}
		raw, err := io.ReadAll(r.Body)
		value, decodeErr := client.DecodeExternalHostedSettlementRequest(raw)
		if err != nil || decodeErr != nil || !value.CountRound || len(value.Runs) != 1 || value.Runs[0].RunID != runID {
			t.Error("MCP settlement changed the tool's explicit intent", err, decodeErr)
		}
		jsonResponse(w, client.ExternalHostedSettlementReceipt{ID: "review_hosted_batch_" + strings.Repeat("b", 32), State: "external_hosted_settled", Runs: 1, RoundCounted: true})
	}))
	defer server.Close()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	const tool = "review_external_hosted_settle"
	args := map[string]any{"pull_number": 2, "idempotency_key": "settle-once", "runs": []any{map[string]any{"run_id": runID, "findings": []any{}}}}
	diagnosticResult(t, call(t, session, tool, args), "invalid_arguments", false)
	args["count_round"] = nil
	diagnosticResult(t, call(t, session, tool, args), "invalid_arguments", false)
	args["count_round"] = true
	args["project_id"] = "other-project"
	diagnosticResult(t, call(t, session, tool, args), "invalid_arguments", false)
	delete(args, "project_id")
	if requests.Load() != 0 {
		t.Fatal("invalid settlement reached the service")
	}
	request := map[string]any{"idempotency_key": "request-once", "expected_configuration_revision": 1, "pull_number": 2, "channel": "hosted", "head_sha": strings.Repeat("b", 40), "wait_seconds": 0}
	refusal := diagnosticResult(t, call(t, session, "review_request", request), "external_hosted_pending", false)
	details, _ := json.Marshal(refusal.Details)
	if !strings.Contains(refusal.Action, tool) || !strings.Contains(string(details), runID) {
		t.Fatal("MCP review refusal lost settlement guidance", refusal)
	}
	var receipt client.ExternalHostedSettlementReceipt
	decodeResult(t, call(t, session, tool, args), &receipt)
	if receipt.Runs != 1 || !receipt.RoundCounted || posts.Load() != 1 {
		t.Fatal("MCP settlement was split or lost its receipt", receipt, posts.Load())
	}
}
