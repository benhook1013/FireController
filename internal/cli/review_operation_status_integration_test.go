package cli_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReviewOperationStatusCLIPublicCompactAndDiagnosticViews(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var statusReads, fullReads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews/operations/") {
			if r.Header.Get("Prefer") != "wait=0" || r.Method != "GET" {
				t.Error("read wait/binding changed")
			}
			if strings.HasSuffix(r.URL.Path, "/status") {
				statusReads.Add(1)
			} else {
				fullReads.Add(1)
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	first := mutationSuccess(t, path, "builder", reviewRequestArgs()...)
	id := first["id"].(string)
	before := f.resolverHit.Load()
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "operation", id)
	var compact client.ReviewOperationStatus
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &compact) != nil || compact.ID != id || compact.Stage != "queued" || compact.NextAction != "wait" || strings.Contains(out, "capability_fingerprint") || strings.Contains(out, "actor_id") {
		t.Fatal(code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, path, "builder", "reviews", "operation", id, "--diagnostics")
	var full client.ReviewOperation
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &full) != nil || full.WorkerID != worker || full.ProjectID != project || full.ID != id {
		t.Fatal(code, out, stderr)
	}
	if statusReads.Load() != 1 || fullReads.Load() != 1 || before != f.resolverHit.Load() {
		t.Fatal("output view polled or contacted provider")
	}
}
