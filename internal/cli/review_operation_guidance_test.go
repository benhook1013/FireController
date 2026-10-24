package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewOperationStatusCLIWaitsForScheduledObservation(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "observation-guidance")
	worker := f.worker(t, project, "Builder")
	id := "review_operation_" + strings.Repeat("a", 32)
	status := client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "hosted", State: "uncertain", Stage: "uncertain", Reason: "hosted_observation_pending", AttemptID: "review_attempt_" + strings.Repeat("b", 32), Revision: 5, UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "contact_controller_owner"}
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+project+"/reviews/operations/"+id+"/status" || r.Header.Get("Prefer") != "wait=0" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != worker {
			t.Error("status guidance changed read semantics")
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "operation", id)
	want := status
	want.NextAction = "wait_for_observation"
	var got client.ReviewOperationStatus
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil || got != want || requests.Load() != 2 {
		t.Fatal("scheduled observation escalated or original facts changed", code, out, stderr, requests.Load())
	}
}
