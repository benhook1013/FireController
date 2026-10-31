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

func TestHostedQueryDiagnosticsMCPExplicitFullRead(t *testing.T) {
	binding := localContext()
	id := "review_operation_" + strings.Repeat("a", 32)
	value := client.ReviewOperation{ID: id, ProjectID: binding.ProjectID, ActorID: "principal_original", WorkerID: binding.WorkerID, ConfigurationRevision: 1, PolicyRevision: 1, CapabilityFingerprint: strings.Repeat("c", 64), PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("d", 40), State: "held", Revision: 3, Reason: "hosted_allowance_unproven", AttemptID: "review_attempt_" + strings.Repeat("b", 32), CreatedAt: "2026-10-30T00:00:00Z", UpdatedAt: "2026-10-30T00:00:10Z"}
	value.HostedQuery = &client.ReviewHostedQueryDiagnostic{QueryID: "review_hosted_query_" + strings.Repeat("e", 32), QueryCommentID: 123, QueryURL: "https://github.com/owner/repo/pull/2#issuecomment-123"}
	reads := 0
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/instance" {
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": binding.InstanceID})
			return
		}
		reads++
		if r.Method != "GET" || r.Header.Get("X-FC-Hosted-Query") != "1" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != binding.WorkerID {
			t.Error("full MCP diagnostics changed read/worker contract")
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer service.Close()
	binding.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential(t)), "chosen")
	var got client.ReviewOperationOutput
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id, "diagnostics": true}), &got)
	if reads != 1 || !reflect.DeepEqual(got.HostedQuery, value.HostedQuery) || got.Diagnostic == nil || !strings.Contains(got.Diagnostic.NextAction, "hosted_query.query_url") || strings.Contains(got.Diagnostic.NextAction, "maintainer") {
		t.Fatal("original source inaccessible through SDK", got, reads)
	}
}
