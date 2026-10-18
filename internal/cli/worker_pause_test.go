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

func TestWorkerPauseCommandsBoundStateAndOriginalReplay(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "pause-cli")
	worker := f.worker(t, p, "Builder")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {p, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "builder", args...)
		if code != cli.ExitSuccess || err != "" {
			t.Fatalf("pause CLI: %d %s %s", code, out, err)
		}
		return out
	}
	var current client.WorkerPause
	if json.Unmarshal([]byte(invoke("workers", "pause-state")), &current) != nil || current.WorkerID != worker || current.Revision != 0 || current.Paused {
		t.Fatal(current)
	}
	code, out, err := mutationInvoke(t, path, "builder", "workers", "resume", "--expect-revision", "1", "--key", "zero-conflict")
	if code != cli.ExitConflict || out != "" || !strings.Contains(err, "worker_pause_revision_conflict") || !strings.Contains(err, `"current_revision":0`) {
		t.Fatal("zero conflict lost", code, out, err)
	}
	args := []string{"workers", "pause", "--reason", "  PRIVATE_REASON\tline\nnext  ", "--expect-revision", "0", "--key", "pause-once"}
	var original client.WorkerPauseReceipt
	raw := invoke(args...)
	if strings.Contains(raw, "PRIVATE_REASON") || json.Unmarshal([]byte(raw), &original) != nil || original.Revision != 1 || original.Outcome != "updated" {
		t.Fatal(raw)
	}
	if json.Unmarshal([]byte(invoke("workers", "pause-state")), &current) != nil || !current.Paused || current.Reason != "  PRIVATE_REASON\tline\nnext  " {
		t.Fatal(current)
	}
	invoke("workers", "resume", "--expect-revision", "1", "--key", "resume-once")
	var replay client.WorkerPauseReceipt
	if json.Unmarshal([]byte(invoke(args...)), &replay) != nil || replay != original {
		t.Fatal("replay changed after resume", replay)
	}
	var history client.Page[client.WorkerPauseHistoryEntry]
	if json.Unmarshal([]byte(invoke("workers", "pause-history", "--limit", "1")), &history) != nil || len(history.Items) != 1 || history.Items[0].Revision != 2 || history.Items[0].Paused || !history.HasMore {
		t.Fatal(history)
	}
	if json.Unmarshal([]byte(invoke("workers", "pause-history", "--limit", "1", "--cursor", history.Cursor)), &history) != nil || len(history.Items) != 1 || history.Items[0].Revision != 1 || !history.Items[0].Paused || history.HasMore {
		t.Fatal(history)
	}
	if f.resolverHit.Load() != 1 {
		t.Fatal("pause commands called provider")
	}
}

func TestWorkerPauseCommandsStrictBeforeProfileIO(t *testing.T) {
	for _, args := range [][]string{
		{"workers", "pause-state", "--worker", "other"}, {"workers", "pause-history", "--limit", "0"}, {"workers", "pause-history", "--cursor", " bad"}, {"workers", "pause-history", "--cursor", ""},
		{"workers", "pause", "--expect-revision", "0", "--key", "k"}, {"workers", "pause", "--reason", "r", "--key", "k"}, {"workers", "pause", "--reason", "r", "--expect-revision", "0"}, {"workers", "pause", "--reason", "\r", "--expect-revision", "0", "--key", "k"}, {"workers", "pause", "--reason", "\u0085", "--expect-revision", "0", "--key", "k"}, {"workers", "pause", "--reason", strings.Repeat("界", 342), "--expect-revision", "0", "--key", "k"},
		{"workers", "resume", "--reason", "", "--expect-revision", "0", "--key", "k"}, {"workers", "resume", "worker-other", "--expect-revision", "0", "--key", "k"}, {"workers", "resume", "--expect-revision", "9223372036854775807", "--key", "k"}, {"workers", "resume", "--expect-revision", "0", "--key", "a", "--key", "b"},
	} {
		code, out, err := run(append([]string{"--profiles-file", "/unavailable/private.json", "--profile", "absent"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatalf("preflight %q: %d %s %s", args, code, out, err)
		}
	}
}

func TestWorkerPauseCommandSharedBudgetAndCancellation(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "pause-deadline")
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
		if strings.HasSuffix(r.URL.Path, "/pause") {
			reads.Add(1)
			<-r.Context().Done()
			return
		}
		t.Error("unexpected fanout")
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {p, worker}})
	started := time.Now()
	code, out, err := mutationInvoke(t, path, "builder", "workers", "pause-state")
	elapsed := time.Since(started)
	if code == cli.ExitSuccess || out != "" || !strings.Contains(err, "deadline_exceeded") || elapsed < 4500*time.Millisecond || elapsed > 6*time.Second || reads.Load() != 1 {
		t.Fatal("budget/fanout", code, out, err, elapsed)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code = cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "builder", "workers", "pause-state"}, nil, &stdout, &stderr)
	if code != cli.ExitCancelled || stdout.Len() != 0 || !strings.Contains(stderr.String(), "cancelled") || reads.Load() != 1 {
		t.Fatal("cancelled command connected", code, stderr.String())
	}
}
