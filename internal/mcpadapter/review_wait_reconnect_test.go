package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestResultWaitRestartCompletesOriginalInvocationThroughSDKAndCLI(t *testing.T) {
	identity := localContext()
	id := "review_operation_" + strings.Repeat("a", 32)
	status := client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "hosted", State: "completed", Revision: 4, Stage: "evidence_ready", AttemptID: "review_attempt_" + strings.Repeat("b", 32), EvidenceID: "review_evidence_" + strings.Repeat("c", 32), UpdatedAt: "2026-10-24T00:00:00Z", NextAction: "read_evidence"}
	count, firstWait := 0, 0
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": identity.InstanceID})
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+identity.ProjectID+"/reviews/operations/"+id+"/status" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Instance") != identity.InstanceID || r.Header.Get("X-FC-Worker") != identity.WorkerID || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("reattachment changed bound identity or submitted work")
		}
		count++
		seconds, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Prefer"), "wait="))
		if err != nil {
			t.Error(err)
		}
		if count%2 == 1 {
			if seconds != 3 {
				t.Error("initial finite wait changed", seconds)
			}
			firstWait = seconds
			w.Header().Set("X-Request-ID", "request-controlled-restart")
			core.WriteError(w, &core.Error{Code: "overloaded", Status: 503, Message: "shutdown", Details: map[string]any{"reason": "result_wait_lifetime"}})
			return
		}
		if seconds < 0 || seconds >= firstWait {
			t.Error("reconnect reset wait budget", seconds, firstWait)
		}
		jsonResponse(w, status)
	}))
	defer service.Close()
	identity.ServerURL = service.URL
	path := profiles(t, map[string]profile.Context{"chosen": identity}, credential(t))
	session := connect(t, adapter(t, path, "chosen"), "")
	var got client.ReviewOperationStatus
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": id, "wait_seconds": 3}), &got)
	if got.ID != id || got.State != "completed" || got.EvidenceID != status.EvidenceID {
		t.Fatal("one SDK invocation lost original completion", got)
	}
	var out, diag bytes.Buffer
	if code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "chosen", "reviews", "operation", id, "--wait-seconds", "3"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || json.Unmarshal(out.Bytes(), &got) != nil || got.ID != id || got.State != "completed" {
		t.Fatal("CLI invocation did not survive restart", code, out.String(), diag.String())
	}
	if count != 4 {
		t.Fatal("restart repeated or lost original invocation", count)
	}
}
