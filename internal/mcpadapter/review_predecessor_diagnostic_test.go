package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestPredecessorDiagnosticSDKReadOnlyOriginalBlocker(t *testing.T) {
	f := mutationFixture(t)
	operationID := "review_operation_" + strings.Repeat("a", 32)
	current := client.ReviewOperationStatus{ID: operationID, PullNumber: 2, Channel: "cli", State: "held", Reason: "predecessor_unfinished", AttemptID: "review_attempt_" + strings.Repeat("b", 32), Revision: 4, Stage: "held", NextAction: "read_diagnostics", UpdatedAt: "2026-01-01T00:00:00Z", SourceRefusal: &client.ReviewSourceRefusal{PullNumber: 6, Cause: "discovery_incomplete"}}
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			f.handler.ServeHTTP(w, r)
			return
		}
		reads++
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+f.project+"/reviews/operations/"+operationID+"/status" || r.Header.Get("X-FC-Source-Refusal") != "v4" {
			t.Error("SDK read changed scope or caused work")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(current)
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "credential")
	if os.WriteFile(token, []byte(f.admin), 0600) != nil {
		t.Fatal("token fixture")
	}
	binding := profile.Context{Profile: "chosen", ServerURL: server.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, token), "chosen")
	var result client.ReviewOperationStatusOutput
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": operationID}), &result)
	if result.Diagnostic == nil || !strings.Contains(result.Diagnostic.Explanation, "predecessor PR #6") || !strings.Contains(result.Diagnostic.NextAction, "reviews pull 6") || !strings.Contains(result.Diagnostic.Explanation, "not necessarily findings") || result.SourceRefusal == nil || result.SourceRefusal.Cause != "discovery_incomplete" {
		t.Fatal("SDK lost original blocker or changed meaning")
	}
	current.SourceRefusal = nil
	result = client.ReviewOperationStatusOutput{}
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": operationID}), &result)
	if result.SourceRefusal != nil || result.Diagnostic == nil || !strings.Contains(result.Diagnostic.Explanation, "not retained") {
		t.Fatal("SDK invented old source facts")
	}
	if reads != 2 {
		t.Fatal("read count changed")
	}
}
