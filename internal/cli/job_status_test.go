package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestJobStatusCLICompactSingleRead(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "status-cli")
	worker := f.worker(t, project, "Builder")
	job := f.call(t, "POST", "/api/v1/projects/"+project+"/jobs", f.admin, worker, map[string]any{"title": "Current task", "brief": "Private resume definition"})["id"].(string)
	f.call(t, "PATCH", "/api/v1/projects/"+project+"/jobs/"+job, f.admin, worker, map[string]any{"expected_revision": 1, "progress": map[string]any{"summary": strings.Repeat("雪", 400), "next": "continue", "blocker": "waiting", "evidence": []string{"private-reference"}}})
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+project+"/jobs/") {
			if r.Method != "GET" || r.URL.Path != "/api/v1/projects/"+project+"/jobs/"+job+"/status" {
				t.Errorf("unexpected status fanout: %s %s", r.Method, r.URL.Path)
			}
			reads.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	code, out, diagnostic := mutationInvoke(t, profiles, "builder", "jobs", "status", job)
	var status client.JobStatus
	if code != cli.ExitSuccess || diagnostic != "" || json.Unmarshal([]byte(out), &status) != nil || status.ID != job || status.Revision != 2 || len(status.ProgressSummary) != 1023 || !status.ProgressTruncated || status.EvidenceCount != 1 || status.Next != "continue" || reads.Load() != 1 {
		t.Fatal(code, out, diagnostic, reads.Load())
	}
	if strings.Contains(out, "Private resume") || strings.Contains(out, "private-reference") || f.resolverHit.Load() != 1 {
		t.Fatal("status leaked private context or performed provider work")
	}
}

func TestJobStatusCLIRejectsArgumentsBeforeProfile(t *testing.T) {
	for _, args := range [][]string{{"jobs", "status"}, {"jobs", "status", "../other"}, {"jobs", "status", "job-one", "job-two"}, {"jobs", "status", "job-one", "--limit", "1"}, {"jobs", "status", "job-one", "--all"}} {
		code, out, diagnostic := mutationInvoke(t, "nonexistent-status-profiles", "builder", args...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_arguments") {
			t.Fatal(args, code, out, diagnostic)
		}
	}
}
