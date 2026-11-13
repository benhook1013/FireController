package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestCLIQuotaWaitMCPPreservesSameIntent(t *testing.T) {
	binding := localContext()
	id := "review_operation_" + strings.Repeat("a", 32)
	wait := &client.ReviewOperationWait{BaselineAttemptID: "review_attempt_" + strings.Repeat("b", 32), RetryNotBefore: "2026-10-24T00:10:00Z", Paused: false}
	status := client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "cli", State: "queued", Stage: "queued", Revision: 5, UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "wait", Wait: wait}
	full := client.ReviewOperation{ID: id, ProjectID: binding.ProjectID, ActorID: "principal_original", WorkerID: binding.WorkerID, ConfigurationRevision: 12, PolicyRevision: 2, CapabilityFingerprint: strings.Repeat("c", 64), PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("d", 40), State: "queued", Revision: 5, CreatedAt: status.UpdatedAt, UpdatedAt: status.UpdatedAt, Wait: wait}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		if r.Method != "GET" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
			t.Error("quota wait guidance changed read authority or requested work")
		}
		w.Header().Set("X-FC-Review-Queued", "cli_quota_wait")
		if strings.HasSuffix(r.URL.Path, "/status") {
			_ = json.NewEncoder(w).Encode(status)
		} else {
			_ = json.NewEncoder(w).Encode(full)
		}
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	var compactOut client.ReviewOperationStatusOutput
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id}), &compactOut)
	var fullOut client.ReviewOperationOutput
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id, "diagnostics": true}), &fullOut)
	if compactOut.NextAction != "wait" || compactOut.State != "queued" || !reflect.DeepEqual(compactOut.Wait, wait) || !reflect.DeepEqual(fullOut.Wait, wait) || !reflect.DeepEqual(fullOut.Diagnostic, compactOut.Diagnostic) {
		t.Fatal("SDK lost queued intent", compactOut, fullOut)
	}
	if compactOut.Diagnostic == nil || !strings.Contains(compactOut.Diagnostic.Explanation, "not the provider reset") || strings.Contains(compactOut.Diagnostic.NextAction, "fresh key") {
		t.Fatal("SDK requires manual rearming", compactOut)
	}
}
