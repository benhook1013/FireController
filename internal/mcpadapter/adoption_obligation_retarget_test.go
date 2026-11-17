package mcpadapter_test

import (
	"encoding/json"
	"math"
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

func TestAdoptionObligationRetargetMCPRevisionBounds(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-retarget-credential"), "chosen")
	t.Run("discovery", func(t *testing.T) {
		listed, err := allDiscoveredTools(t, session)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range listed.Tools {
			if tool.Name != "review_obligation_retarget" {
				continue
			}
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties map[string]struct {
					Type             string
					Minimum, Maximum json.Number
				}
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			revision := schema.Properties["expected_revision"]
			maximum, err := revision.Maximum.Float64()
			if err != nil || revision.Type != "integer" || revision.Minimum.String() != "1" || maximum != float64(math.MaxInt64-1) {
				t.Fatal("discovery revision bounds disagree with retarget contract", revision)
			}
			target := schema.Properties["target_pull_number"]
			if target.Minimum.String() != "0" || target.Maximum.String() != "2147483647" {
				t.Fatal("explicit backlog target bounds changed", target)
			}
			return
		}
		t.Fatal("retarget missing from discovery")
	})
	for _, revision := range []int64{-1, 0, math.MaxInt64} {
		args := map[string]any{"obligation_id": "adoption_obligation_" + strings.Repeat("a", 32), "expected_revision": revision, "target_pull_number": 0, "reason": "Keep receiving work open", "idempotency_key": "retarget-bounds"}
		result := diagnosticResult(t, call(t, session, "review_obligation_retarget", args), "invalid_arguments", false)
		if result.Message != "Use only the declared tool fields with valid bounded values and required keys or revisions." {
			t.Errorf("revision %d reached native document validation instead of argument bounds: %s", revision, result.Message)
		}
	}
	for _, revision := range []int64{1, math.MaxInt64 - 1} {
		args := map[string]any{"obligation_id": "adoption_obligation_" + strings.Repeat("a", 32), "expected_revision": revision, "target_pull_number": 0, "reason": "Keep receiving work open", "idempotency_key": "retarget-bounds"}
		// Accepted bounds proceed to credential loading, never to HTTP.
		diagnosticResult(t, call(t, session, "review_obligation_retarget", args), "credential_unreadable", false)
	}
	if requests.Load() != 0 {
		t.Fatal("revision validation contacted service", requests.Load())
	}
}
