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

func TestAdoptionObligationRetargetMCPExplicitBacklogReceiver(t *testing.T) {
	binding := localContext()
	id := "adoption_obligation_" + strings.Repeat("a", 32)
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/"+binding.ProjectID+"/reviews/adoption-obligations/"+id+"/retarget" || r.Header.Get("Idempotency-Key") != "retarget-same" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
			t.Error("retarget changed operation or own binding", r.Method, r.URL)
		}
		var body client.AdoptionObligationRetargetRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.TargetPullNumber == nil || *body.TargetPullNumber != 0 || body.ExpectedRevision == nil || *body.ExpectedRevision != 2 || body.Reason != "Keep receiving work open" {
			t.Error("explicit zero receiver missing", body)
		}
		posts.Add(1)
		_ = json.NewEncoder(w).Encode(client.AdoptionObligationRetargetReceipt{ID: id, Status: "open", Revision: 3, TargetPullNumber: 0, AffectedPullNumbers: []int64{2}, Outcome: "retargeted"})
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	args := map[string]any{"obligation_id": id, "expected_revision": 2, "target_pull_number": 0, "reason": "Keep receiving work open", "idempotency_key": "retarget-same"}
	var out client.AdoptionObligationRetargetReceipt
	decodeResult(t, call(t, session, "review_obligation_retarget", args), &out)
	if out.Status != "open" || out.Revision != 3 || out.TargetPullNumber != 0 || posts.Load() != 1 {
		t.Fatal("MCP retarget resolved or duplicated work", out, posts.Load())
	}
	for _, field := range []string{"target_pull_number", "expected_revision", "reason"} {
		bad := map[string]any{}
		for k, v := range args {
			bad[k] = v
		}
		delete(bad, field)
		diagnosticResult(t, call(t, session, "review_obligation_retarget", bad), "invalid_arguments", false)
	}
	if posts.Load() != 1 {
		t.Fatal("invalid retarget reached service")
	}
}
