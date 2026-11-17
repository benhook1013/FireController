package client_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewCIDiagnosticsBoundedSelectedContextsAndValidation(t *testing.T) {
	v := sampleCIAssessment()
	v.State, v.Reason, v.Satisfied, v.Pending = "pending", "ci_required_checks_pending", 0, 1
	checks := []client.ReviewCIContext{{Context: "build", AppID: 15368, State: "pending", Reason: "ci_status_pending", CheckRun: &client.ReviewCICheckRun{ID: 300, Status: "completed", Conclusion: "success"}, CommitStatus: &client.ReviewCICommitStatus{ID: 991, State: "pending"}}}
	v.Checks = &checks
	var response atomic.Value
	put := func(v any) { b, _ := json.Marshal(v); response.Store(b) }
	put(v)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/source/2/ci" || r.URL.RawQuery != "diagnostics=true" || r.Header.Get("Prefer") != "" {
			t.Error("diagnostics changed scope or requested provider work")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response.Load().([]byte))
	}))
	defer s.Close()
	c := newClient(t, s)
	if got, err := c.ReviewSourceCIDiagnostics(t.Context(), 2); err != nil || got.Checks == nil || (*got.Checks)[0].CommitStatus.State != "pending" {
		t.Fatal(got, err)
	}
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { delete(m, "checks") },
		func(m map[string]any) { m["checks"] = nil },
		func(m map[string]any) { m["checks"] = []any{} },
		func(m map[string]any) { m["checks"].([]any)[0].(map[string]any)["state"] = "satisfied" },
		func(m map[string]any) {
			m["checks"].([]any)[0].(map[string]any)["check_run"].(map[string]any)["id"] = 0
		},
		func(m map[string]any) { m["checks"].([]any)[0].(map[string]any)["raw_provider_text"] = "private" },
	} {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mutate(m)
		put(m)
		_, err := c.ReviewSourceCIDiagnostics(t.Context(), 2)
		checkError(t, err, "invalid_response")
	}
	// The explicit read accepts the supported worst-case context names without
	// enlarging the ordinary assessment's 16 KiB response budget.
	checks = make([]client.ReviewCIContext, 128)
	for i := range checks {
		checks[i] = client.ReviewCIContext{Context: fmt.Sprintf("%03d", i) + strings.Repeat("<", 252), State: "pending", Reason: "ci_status_pending", CommitStatus: &client.ReviewCICommitStatus{ID: int64(i + 1), State: "pending"}}
	}
	v.Checks, v.Required, v.Pending = &checks, 128, 128
	put(v)
	if got, err := c.ReviewSourceCIDiagnostics(t.Context(), 2); err != nil || len(*got.Checks) != 128 {
		t.Fatal("maximum context details refused", err)
	}
}
