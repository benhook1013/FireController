package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/mcpadapter"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestWaitLifetimeDiagnosticPreservedThroughCLIAndRegisteredMCP(t *testing.T) {
	identity := localContext()
	var reads atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": identity.InstanceID})
			return
		}
		reads.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/ci/watch" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("wait changed its read binding or request type")
		}
		w.Header().Set("X-Request-ID", "request-original-wait")
		core.WriteError(w, &core.Error{Code: "overloaded", Status: 503, Message: "PRIVATE_SERVER_MESSAGE", Details: map[string]string{"reason": "result_wait_lifetime"}})
	}))
	defer s.Close()
	identity.ServerURL = s.URL
	path := profiles(t, map[string]profile.Context{"chosen": identity}, credential(t))
	session := connect(t, adapter(t, path, "chosen"), "")
	head := strings.Repeat("a", 40)
	got := diagnosticResult(t, call(t, session, "wait_for_ci", map[string]any{"targets": []map[string]any{{"pull_number": 2, "head_sha": head}}, "wait_seconds": 300}), "overloaded", false)
	check := func(got mcpadapter.Diagnostic) {
		t.Helper()
		details, ok := got.Details.(map[string]any)
		if !ok || details["reason"] != "result_wait_lifetime" || got.RequestID != "request-original-wait" || got.OutcomeUnknown || !strings.Contains(got.Message, "shutdown") || !strings.Contains(got.Action, "CI PR/head/base pins") || !strings.Contains(got.Action, "does not mean CI failed") {
			t.Fatal("adapter lost proven shutdown diagnosis", got)
		}
		if strings.Contains(got.Message+got.Action, "PRIVATE_SERVER") {
			t.Fatal("adapter echoed private server text")
		}
	}
	check(got)
	var out, diag bytes.Buffer
	args := []string{"--profiles-file", path, "--profile", "chosen", "reviews", "ci-wait", "--target", "2:" + head, "--wait-seconds", "300"}
	if code := cli.RunContext(t.Context(), args, nil, &out, &diag); code != cli.ExitService || out.Len() != 0 {
		t.Fatal("read shutdown changed CLI exit semantics", code, out.String(), diag.String())
	}
	var value struct {
		Error mcpadapter.Diagnostic `json:"error"`
	}
	if json.Unmarshal(diag.Bytes(), &value) != nil {
		t.Fatal("invalid CLI diagnostic", diag.String())
	}
	check(value.Error)
	if reads.Load() != 2 {
		t.Fatal("adapter started automatic retries", reads.Load())
	}
}
