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

func TestReviewOperationStatusMCPWaitsForScheduledObservation(t *testing.T) {
	binding := localContext()
	id := "review_operation_" + strings.Repeat("a", 32)
	status := client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "hosted", State: "uncertain", Stage: "uncertain", Reason: "hosted_observation_pending", AttemptID: "review_attempt_" + strings.Repeat("b", 32), Revision: 5, UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "contact_controller_owner"}
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+binding.ProjectID+"/reviews/operations/"+id+"/status" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
			t.Error("SDK status guidance changed read semantics")
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	var got client.ReviewOperationStatus
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id}), &got)
	want := status
	want.NextAction = "wait_for_observation"
	if got != want || requests.Load() != 2 {
		t.Fatal("SDK scheduled observation escalated or original facts changed", got, requests.Load())
	}
}
