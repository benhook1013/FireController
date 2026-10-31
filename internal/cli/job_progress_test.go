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

func TestUnifiedJobCommandsPartialClearReplayAndExplicitArchive(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "cli-current-progress")
	worker := f.worker(t, project, "Builder")
	job := f.call(t, "POST", "/api/v1/projects/"+project+"/jobs", f.admin, worker, map[string]any{"title": "Job", "brief": "Standing brief"})["id"].(string)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, profiles, "builder", args...)
		if code != cli.ExitSuccess || err != "" {
			t.Fatalf("CLI failed: %d %s", code, err)
		}
		return out
	}
	update := []string{"jobs", "update", job, "--expect-revision", "1", "--key", "progress-once", "--progress", "-- literal progress\n<text>", "--next", "Next", "--evidence", "proof:one"}
	first := invoke(update...)
	if first != invoke(update...) || strings.Contains(first, "literal progress") {
		t.Fatal("receipt replay/body echo")
	}
	var receipt client.JobReceipt
	if json.Unmarshal([]byte(first), &receipt) != nil || receipt.Revision != 2 || receipt.Outcome != "updated" {
		t.Fatal("bad receipt")
	}
	var current client.Job
	if json.Unmarshal([]byte(invoke("jobs", "read", job)), &current) != nil || current.Progress.Summary != "-- literal progress\n<text>" || current.Progress.Next != "Next" || current.Brief != "Standing brief" || current.Notes.Items == nil || current.Checklist.Remaining == nil {
		t.Fatal("default detail not complete")
	}
	invoke("jobs", "update", job, "--expect-revision", "2", "--key", "clear-evidence", "--evidence=", "--blocker", "Blocked")
	invoke("jobs", "update", job, "--expect-revision", "3", "--key", "clear-summary", "--progress=", "--next=")
	if json.Unmarshal([]byte(invoke("jobs", "read", job)), &current) != nil || current.Progress.Summary != "" || current.Progress.Next != "" || current.Progress.Blocker != "Blocked" || len(current.Progress.Evidence) != 0 {
		t.Fatal("partial clear lost omitted blocker")
	}
	var noop client.JobReceipt
	if json.Unmarshal([]byte(invoke("jobs", "update", job, "--expect-revision", "4", "--key", "noop", "--blocker", "Blocked")), &noop) != nil || noop.Revision != 4 || noop.Outcome != "unchanged" {
		t.Fatal("noop changed revision")
	}
	var history client.Page[client.ProgressHistoryEntry]
	if json.Unmarshal([]byte(invoke("jobs", "progress-history", job, "--limit", "1")), &history) != nil || len(history.Items) != 1 || !history.HasMore || history.Items[0].JobRevision != 4 {
		t.Fatal("explicit archive first page")
	}
	if json.Unmarshal([]byte(invoke("jobs", "progress-history", job, "--limit", "1", "--cursor", history.Cursor)), &history) != nil || len(history.Items) != 1 || history.Items[0].JobRevision != 3 {
		t.Fatal("explicit continuation")
	}
	code, out, err := mutationInvoke(t, profiles, "builder", "jobs", "update", job, "--expect-revision", "1", "--key", "stale", "--progress", "stale")
	if code != cli.ExitConflict || out != "" || !strings.Contains(err, "revision_conflict") {
		t.Fatal("CAS refused incorrectly")
	}
	var conflict struct {
		Error struct {
			Action         string `json:"action"`
			OutcomeUnknown bool   `json:"outcome_unknown"`
			Details        struct {
				CurrentRevision int64 `json:"current_revision"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(err), &conflict) != nil || conflict.Error.OutcomeUnknown || conflict.Error.Details.CurrentRevision != 4 || !strings.Contains(conflict.Error.Action, "preserve concurrent edits") || !strings.Contains(conflict.Error.Action, "current revision") || !strings.Contains(conflict.Error.Action, "new idempotency key") || strings.Contains(conflict.Error.Action, "current access") {
		t.Fatal("guarded conflict lost safe edit recovery")
	}
	if f.resolverHit.Load() != 1 {
		t.Fatal("job operations contacted provider")
	}
}
func TestUnifiedJobArgumentsBeforeCredentials(t *testing.T) {
	base := []string{"jobs", "update", "job-one", "--key", "key", "--expect-revision", "1"}
	cases := [][]string{base, append(append([]string{}, base...), "--body", "obsolete"), append(append([]string{}, base...), "--evidence=", "--evidence", "proof"), append(append([]string{}, base...), "--progress", "bad\x00"), append(append([]string{}, base...), "--progress", "one", "--progress", "two"), {"jobs", "edit", "job-one"}, {"jobs", "checkpoint", "job-one"}, {"jobs", "checkpoint-latest", "job-one"}, {"jobs", "checkpoints", "job-one"}, {"jobs", "context", "job-one"}, {"jobs", "updates", "job-one"}, {"jobs", "progress-history", "job-one", "--limit", "101"}}
	for _, args := range cases {
		code, out, err := run(append([]string{"--profile", "absent", "--profiles-file", "/missing/profiles.json"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatalf("invalid command reached credentials: %v %d %s", args, code, err)
		}
	}
}

func TestUnifiedJobCommandSharedDeadlineAndPrecancel(t *testing.T) {
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
	code, out, err := mutationInvoke(t, path, "bound", "jobs", "read", "job-one")
	elapsed := time.Since(started)
	if code == cli.ExitSuccess || out != "" || !strings.Contains(err, "deadline_exceeded") || strings.Contains(err, "outcome_unknown") || reads.Load() != 1 || elapsed < 4500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatal(code, out, err, reads.Load(), elapsed)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code = cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "bound", "jobs", "read", "job-one"}, nil, &stdout, &stderr)
	if code != cli.ExitCancelled || stdout.Len() != 0 || reads.Load() != 1 {
		t.Fatal("canceled context connected", code, stderr.String())
	}
}
