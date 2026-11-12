package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/mcpadapter"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestWaitLifetimeDiagnosticPreservedThroughCLIAndRegisteredMCP(t *testing.T) {
	identity := localContext()
	operationID := "review_operation_" + strings.Repeat("f", 32)
	var reads atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": identity.InstanceID})
			return
		}
		reads.Add(1)
		ci := r.Method == "POST" && r.URL.Path == "/api/v1/projects/project-one/reviews/ci/watch"
		operation := r.Method == "GET" && (r.URL.Path == "/api/v1/projects/project-one/reviews/operations/"+operationID || r.URL.Path == "/api/v1/projects/project-one/reviews/operations/"+operationID+"/status")
		if (!ci && !operation) || r.Header.Get("Idempotency-Key") != "" {
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
	base := strings.Repeat("b", 40)
	pins := []client.CIWatchTarget{{PullNumber: 2, HeadSHA: head, BaseSHA: base}, {PullNumber: 7, HeadSHA: strings.Repeat("c", 40)}}
	got := diagnosticResult(t, call(t, session, "wait_for_ci", map[string]any{"targets": pins, "wait_seconds": 0}), "overloaded", false)
	check := func(got mcpadapter.Diagnostic) {
		t.Helper()
		details, ok := got.Details.(map[string]any)
		if !ok || details["reason"] != "result_wait_lifetime" || got.RequestID != "request-original-wait" || got.OutcomeUnknown || !strings.Contains(got.Message, "shutdown") || !strings.Contains(got.Action, "at least one base was implicit") || !strings.Contains(got.Action, "does not mean CI failed") {
			t.Fatal("adapter lost proven shutdown diagnosis", got)
		}
		if strings.Contains(got.Message+got.Action, "PRIVATE_SERVER") {
			t.Fatal("adapter echoed private server text")
		}
		var actual []client.CIWatchTarget
		raw, _ := json.Marshal(details["targets"])
		if json.Unmarshal(raw, &actual) != nil || !reflect.DeepEqual(actual, pins) || len(details) != 2 {
			t.Fatal("interrupted CI wait lost original pins", got)
		}
	}
	check(got)
	var out, diag bytes.Buffer
	args := []string{"--profiles-file", path, "--profile", "chosen", "reviews", "ci-wait", "--target", "2:" + head + ":" + base, "--target", "7:" + pins[1].HeadSHA, "--wait-seconds", "0"}
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
	checkOperation := func(got mcpadapter.Diagnostic) {
		t.Helper()
		details, ok := got.Details.(map[string]any)
		if !ok || len(details) != 2 || details["operation_id"] != operationID || details["reason"] != "result_wait_lifetime" || got.RequestID != "request-original-wait" || got.OutcomeUnknown || !strings.Contains(got.Message, "shutdown") {
			t.Fatal("interrupted operation lost original identity", got)
		}
	}
	for _, full := range []bool{false, true} {
		checkOperation(diagnosticResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": operationID, "wait_seconds": 0, "diagnostics": full}), "overloaded", false))
		args := []string{"--profiles-file", path, "--profile", "chosen", "reviews", "operation", operationID, "--wait-seconds", "0"}
		if full {
			args = append(args, "--diagnostics")
		}
		out.Reset()
		diag.Reset()
		if code := cli.RunContext(t.Context(), args, nil, &out, &diag); code != cli.ExitService || out.Len() != 0 || json.Unmarshal(diag.Bytes(), &value) != nil {
			t.Fatal("operation interruption changed CLI outcome", code, out.String(), diag.String())
		}
		checkOperation(value.Error)
	}
	if reads.Load() != 6 {
		t.Fatal("adapter started automatic retries", reads.Load())
	}
}
