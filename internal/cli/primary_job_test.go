package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestPrimaryJobCommandsUseBoundFocusAndRecoverOriginalKey(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "primary-cli")
	worker := f.worker(t, p, "Builder")
	job := f.call(t, "POST", "/api/v1/projects/"+p+"/jobs", f.admin, worker, map[string]any{"title": "Explicit focus", "brief": "PRIVATE_BRIEF", "worker_id": worker})["id"].(string)
	var lose atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/primary-job") {
			writes.Add(1)
			if lose.CompareAndSwap(true, false) {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != 200 {
					t.Errorf("focus did not commit: %d", actual.Code)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(actual.Code)
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {p, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "builder", args...)
		if code != cli.ExitSuccess || err != "" {
			t.Fatalf("primary CLI: %d %s %s", code, out, err)
		}
		return out
	}
	var focus client.PrimaryJob
	if json.Unmarshal([]byte(invoke("jobs", "primary")), &focus) != nil || focus.WorkerID != worker || focus.Revision != 0 || focus.JobID != "" {
		t.Fatal(focus)
	}
	code, out, err := mutationInvoke(t, path, "builder", "jobs", "primary-clear", "--expect-revision", "1", "--key", "initial-stale")
	if code != cli.ExitConflict || out != "" || !strings.Contains(err, "primary_job_revision_conflict") || !strings.Contains(err, `"current_revision":0`) {
		t.Fatal("initial focus conflict lost revision zero", code, out, err)
	}
	beforeLost := writes.Load()
	set := []string{"jobs", "primary-set", job, "--expect-revision", "0", "--key", "primary-once"}
	lose.Store(true)
	code, out, err = mutationInvoke(t, path, "builder", set...)
	if code == cli.ExitSuccess || out != "" || !strings.Contains(err, `"outcome_unknown":true`) || writes.Load() != beforeLost+1 {
		t.Fatal("uncertain PUT was retried or acknowledged", code, out, err)
	}
	var receipt client.PrimaryJobReceipt
	if json.Unmarshal([]byte(invoke(set...)), &receipt) != nil || receipt.ID != worker || receipt.Revision != 1 || receipt.Outcome != "updated" {
		t.Fatal(receipt)
	}
	if json.Unmarshal([]byte(invoke("jobs", "primary")), &focus) != nil || focus.JobID != job || focus.Revision != 1 {
		t.Fatal(focus)
	}
	clear := invoke("jobs", "primary-clear", "--expect-revision", "1", "--key", "primary-clear")
	if strings.Contains(clear, "PRIVATE_BRIEF") || strings.Contains(clear, job) {
		t.Fatal("receipt expanded job context")
	}
	var replay client.PrimaryJobReceipt
	if json.Unmarshal([]byte(invoke(set...)), &replay) != nil || replay != receipt {
		t.Fatal("original receipt changed after clear", replay)
	}
	var history client.Page[client.PrimaryJobHistoryEntry]
	if json.Unmarshal([]byte(invoke("jobs", "primary-history", "--limit", "1")), &history) != nil || len(history.Items) != 1 || history.Items[0].Revision != 2 || history.Items[0].Reason != "cleared" || !history.HasMore {
		t.Fatal(history)
	}
	if json.Unmarshal([]byte(invoke("jobs", "primary-history", "--limit", "1", "--cursor", history.Cursor)), &history) != nil || len(history.Items) != 1 || history.Items[0].Revision != 1 || history.Items[0].JobID != job || history.HasMore {
		t.Fatal(history)
	}
	code, out, err = mutationInvoke(t, path, "builder", "jobs", "primary-set", job, "--expect-revision", "1", "--key", "stale")
	if code != cli.ExitConflict || out != "" || !strings.Contains(err, "primary_job_revision_conflict") || !strings.Contains(err, `"current_revision":2`) {
		t.Fatal("focus conflict details lost", code, out, err)
	}
	if f.resolverHit.Load() != 1 {
		t.Fatal("focus commands performed provider work")
	}
}

func TestPrimaryJobCommandsValidateBeforeProfileIO(t *testing.T) {
	for _, args := range [][]string{
		{"jobs", "primary", "job-one"}, {"jobs", "primary", "--worker", "bad/worker"}, {"jobs", "primary-history", "--limit", "0"}, {"jobs", "primary-history", "--limit", "101"}, {"jobs", "primary-history", "--cursor", " bad"}, {"jobs", "primary-history", "--cursor", ""}, {"jobs", "primary-history", "--limit", "1", "--limit", "2"},
		{"jobs", "primary-set", "job-one", "--key", "explicit"}, {"jobs", "primary-set", "job-one", "--expect-revision", "0"}, {"jobs", "primary-set", "bad/job", "--expect-revision", "0", "--key", "explicit"}, {"jobs", "primary-set", "", "--expect-revision", "0", "--key", "explicit"},
		{"jobs", "primary-clear", "job-one", "--expect-revision", "0", "--key", "explicit"}, {"jobs", "primary-clear", "--expect-revision", "-1", "--key", "explicit"}, {"jobs", "primary-clear", "--expect-revision", "9223372036854775807", "--key", "explicit"}, {"jobs", "primary-clear", "--expect-revision", "0", "--key", "a", "--key", "b"}, {"jobs", "primary-clear", "--expect-revision", "0", "--key", "explicit", "--worker", "bad/worker"},
	} {
		code, out, err := run(append([]string{"--profiles-file", "/unavailable/private.json", "--profile", "absent"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatalf("preflight %q: %d %s %s", args, code, out, err)
		}
	}
}

func TestPrimaryJobCommandSharesFiveSecondBudgetAndCancellation(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "primary-deadline")
	worker := f.worker(t, p, "Builder")
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
		if strings.HasSuffix(r.URL.Path, "/primary-job") {
			reads.Add(1)
			<-r.Context().Done()
			return
		}
		t.Error("unexpected command fanout")
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {p, worker}})
	start := time.Now()
	code, out, err := mutationInvoke(t, path, "builder", "jobs", "primary")
	elapsed := time.Since(start)
	if code == cli.ExitSuccess || out != "" || !strings.Contains(err, "deadline_exceeded") || elapsed < 4500*time.Millisecond || elapsed > 6*time.Second || reads.Load() != 1 {
		t.Fatal("aggregate budget or no-fanout failed", code, out, err, elapsed)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code = cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "builder", "jobs", "primary"}, nil, &stdout, &stderr)
	if code != cli.ExitCancelled || stdout.Len() != 0 || !strings.Contains(stderr.String(), "cancelled") || reads.Load() != 1 {
		t.Fatal("canceled command connected", code, stderr.String())
	}
}
