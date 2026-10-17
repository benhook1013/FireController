package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestJobContextCommandSingleCombinedReadAndPreviewDisclosure(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "context-cli")
	caller := f.worker(t, project, "Caller")
	other := f.worker(t, project, "Other")
	job := f.call(t, "POST", "/api/v1/projects/"+project+"/jobs", f.admin, caller, map[string]any{"title": "Selected job", "brief": "Authoritative brief", "worker_id": other})["id"].(string)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == "GET" && r.URL.Path != "/api/v1/instance" && (!strings.HasSuffix(r.URL.Path, "/context") || r.Header.Get("X-FC-Worker") != caller) {
			t.Error("context fanned out/binding changed", r.URL)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"caller": {project, caller}})
	code, out, diagnostic := mutationInvoke(t, path, "caller", "jobs", "context", job)
	var got client.JobContextResult
	if code != cli.ExitSuccess || diagnostic != "" || json.Unmarshal([]byte(out), &got) != nil || !got.Complete || got.Checkpoint != nil || got.Job.ID != job || got.Job.WorkerID != other || got.WorkerID != caller || requests.Load() != 2 {
		t.Fatal("combined command", code, diagnostic, got, requests.Load())
	}
	if code, _, diag := mutationInvoke(t, path, "caller", "notes", "add", "--body", "Short preview", "--key", "context-note"); code != cli.ExitSuccess {
		t.Fatal(diag)
	}
	var before int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	start := requests.Load()
	code, out, diagnostic = mutationInvoke(t, path, "caller", "jobs", "context", job, "--limit", "2")
	if code != cli.ExitSuccess || diagnostic != "" || json.Unmarshal([]byte(out), &got) != nil || got.Complete || len(got.Omitted) != 1 || got.Omitted[0] != "note_bodies" || got.Notes.Items[0].BodyTruncated || got.Notes.Items[0].BodyPreview != "Short preview" || requests.Load() != start+2 {
		t.Fatal("short preview implied full content/fanout", code, diagnostic, got)
	}
	var after int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&after); err != nil || after != before || f.resolverHit.Load() != 1 {
		t.Fatal("context wrote/called provider", before, after, err)
	}
}
func TestJobContextCommandRejectsInvalidArgumentsBeforeProfileIO(t *testing.T) {
	for _, args := range [][]string{{"jobs", "context"}, {"jobs", "context", "../job"}, {"jobs", "context", "job", "other"}, {"jobs", "context", "job", "--limit", "0"}, {"jobs", "context", "job", "--limit", "101"}, {"jobs", "context", "job", "--limit", "1.0"}, {"jobs", "context", "job", "--limit", "1", "--limit", "2"}, {"jobs", "context", "job", "--cursor", "next"}, {"jobs", "context", "job", "--worker", "other"}, {"jobs", "context", "job", "--limit"}} {
		code, out, err := run(append([]string{"--profiles-file", "/unavailable/private", "--profile", "missing"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatal(args, code, out, err)
		}
	}
}
func TestJobContextCommandSharedDeadlineAndPrecancel(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "context-budget")
	worker := f.worker(t, project, "Bound")
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			select {
			case <-time.After(150 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			f.handler.ServeHTTP(w, r)
			return
		}
		reads.Add(1)
		<-r.Context().Done()
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"bound": {project, worker}})
	started := time.Now()
	code, out, err := mutationInvoke(t, path, "bound", "jobs", "context", "job-one")
	elapsed := time.Since(started)
	if code == cli.ExitSuccess || out != "" || !strings.Contains(err, "deadline_exceeded") || strings.Contains(err, "outcome_unknown") || reads.Load() != 1 || elapsed < 4500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatal(code, out, err, reads.Load(), elapsed)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code = cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "bound", "jobs", "context", "job-one"}, nil, &stdout, &stderr)
	if code != cli.ExitCancelled || stdout.Len() != 0 || reads.Load() != 1 {
		t.Fatal("canceled context connected", code, stderr.String())
	}
}
