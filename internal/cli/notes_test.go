package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestNotesCommandsDefaultsPartialStateAndExplicitArchives(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "notes-cli")
	worker := f.worker(t, p, "Builder")
	other := f.worker(t, p, "Other")
	var patches []map[string]any
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" && strings.Contains(r.URL.Path, "/notes/") {
			raw, _ := ioReadNotes(r)
			patches = append(patches, raw)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {p, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "builder", args...)
		if code != cli.ExitSuccess || err != "" {
			t.Fatalf("notes CLI %v: %d %s %s", args, code, out, err)
		}
		return out
	}
	var receipt client.NoteReceipt
	if json.Unmarshal([]byte(invoke("notes", "add", "--body", "-- Markdown\t exact\n", "--key", "note-create")), &receipt) != nil || receipt.Revision != 1 {
		t.Fatal(receipt)
	}
	var current client.Note
	if json.Unmarshal([]byte(invoke("notes", "read", receipt.ID)), &current) != nil || current.WorkerID != worker || current.Kind != "reminder" || current.Body != "-- Markdown\t exact\n" {
		t.Fatal(current)
	}
	invoke("notes", "update", receipt.ID, "--expect-revision", "1", "--key", "note-consumed", "--status", "consumed")
	if len(patches) != 1 || patches[0]["body"] != nil || len(patches[0]) != 2 {
		t.Fatal("status required body echo", patches)
	}
	var history client.Page[client.NoteHistorySummary]
	if json.Unmarshal([]byte(invoke("notes", "history", receipt.ID, "--limit", "1")), &history) != nil || history.Items[0].Revision != 2 || !history.HasMore || history.Items[0].ActorWorkerID != worker {
		t.Fatal(history)
	}
	if json.Unmarshal([]byte(invoke("notes", "revision", receipt.ID, "1")), &current) != nil || current.Status != "pending" || current.Body != "-- Markdown\t exact\n" {
		t.Fatal(current)
	}
	var noop client.NoteReceipt
	if json.Unmarshal([]byte(invoke("notes", "update", receipt.ID, "--expect-revision", "2", "--key", "same-state", "--status", "consumed")), &noop) != nil || noop.Outcome != "unchanged" || noop.Revision != 2 {
		t.Fatal(noop)
	}
	code, out, err := mutationInvoke(t, path, "builder", "notes", "update", receipt.ID, "--expect-revision", "1", "--key", "stale", "--status", "consumed")
	if code != cli.ExitConflict || out != "" || !strings.Contains(err, "note_revision_conflict") {
		t.Fatal(code, out, err)
	}
	var job client.JobReceipt
	json.Unmarshal([]byte(invoke("jobs", "create", "--title", "Assigned work", "--brief", "Same brief", "--worker", worker, "--key", "notes-job")), &job)
	invoke("notes", "add", "--worker", other, "--job", job.ID, "--body", "Assigned job reminder", "--key", "assigned-note")
	var page client.Page[client.NoteSummary]
	if json.Unmarshal([]byte(invoke("notes", "list")), &page) != nil || len(page.Items) != 1 || page.Items[0].WorkerID != other {
		t.Fatal("assigned expansion", page)
	}
	if json.Unmarshal([]byte(invoke("notes", "list", "--direct-only")), &page) != nil || len(page.Items) != 0 {
		t.Fatal("direct-only expanded", page)
	}
	if json.Unmarshal([]byte(invoke("notes", "list", "--worker", other, "--direct-only")), &page) != nil || len(page.Items) != 1 || page.Items[0].WorkerID != other {
		t.Fatal("explicit worker", page)
	}
	invoke("notes", "add", "--clear-worker", "--phase", "Later", "--kind", "source", "--body", "Phase-only", "--key", "phase-note")
	if json.Unmarshal([]byte(invoke("notes", "list", "--all-workers", "--status", "all")), &page) != nil || len(page.Items) != 3 {
		t.Fatal("project list", page)
	}
	// Full escaped body file is local/private, with no extra profile identity.
	body := strings.Repeat("<", 16<<10)
	file := filepath.Join(t.TempDir(), "body.md")
	if os.WriteFile(file, []byte(body), 0600) != nil {
		t.Fatal("write fixture")
	}
	var fromFile client.NoteReceipt
	json.Unmarshal([]byte(invoke("notes", "add", "--body-file", file, "--key", "body-file")), &fromFile)
	if json.Unmarshal([]byte(invoke("notes", "read", fromFile.ID)), &current) != nil || current.Body != body {
		t.Fatal("file body changed")
	}
	if f.resolverHit.Load() != 1 {
		t.Fatal("notes used provider")
	}
}

// Keep handler inspection independent of the production parser and restore the
// exact body for the actual public handler.
func ioReadNotes(r *http.Request) (map[string]any, error) {
	var out map[string]any
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	err = json.Unmarshal(raw, &out)
	return out, err
}
func TestNotesCommandsRejectArgumentsBeforeProfilesOrFiles(t *testing.T) {
	for _, args := range [][]string{
		{"notes", "add", "--body", "x", "--clear-worker", "--key", "k"}, {"notes", "add", "--body", "x", "--body-file", "/unavailable/body", "--key", "k"}, {"notes", "add", "--body-file", "/unavailable/body", "--kind", "wrong", "--key", "k"}, {"notes", "add", "--worker", "x", "--clear-worker", "--body", "x", "--key", "k"}, {"notes", "add", "--body", string([]byte{0xff}), "--key", "k"}, {"notes", "add", "--body", "\r", "--key", "k"}, {"notes", "add", "--body", "x", "--key", "a", "--key", "b"},
		{"notes", "update", "n", "--status", "consumed", "--expect-revision", "0", "--key", "k"}, {"notes", "update", "n", "--expect-revision", "1", "--key", "k"}, {"notes", "update", "n", "--expect-revision", "1", "--key", "k", "--status", "dismissed"}, {"notes", "update", "n", "--expect-revision", "1", "--key", "k", "--job", "j", "--clear-job"}, {"notes", "list", "--all-workers", "--worker", "other"}, {"notes", "list", "--all-workers", "--direct-only"}, {"notes", "list", "--all-workers=false"}, {"notes", "list", "--status", ""}, {"notes", "list", "--limit", "0"}, {"notes", "list", "--cursor", " bad"}, {"notes", "read", "../n"}, {"notes", "revision", "n", "0"}, {"notes", "read", "n", "--unknown"},
	} {
		code, out, err := run(append([]string{"--profiles-file", "/unavailable/profiles", "--profile", "chosen"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatalf("args %q: %d %s %s", args, code, out, err)
		}
	}
}
func TestNotesCommandsPrecancelAndOutputFailureHonest(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "notes-cancel")
	worker := f.worker(t, p, "Builder")
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); f.handler.ServeHTTP(w, r) }))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {p, worker}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "builder", "notes", "list"}, nil, &stdout, &stderr)
	if code != cli.ExitCancelled || stdout.Len() != 0 || requests.Load() != 0 {
		t.Fatal(code, stderr.String(), requests.Load())
	}
	// A failed stdout after committed mutation must retain original-key recovery.
	stderr.Reset()
	code = cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "builder", "notes", "add", "--body", "output", "--key", "output-once"}, nil, noteFailWriter{}, &stderr)
	if code != cli.ExitConfig || !strings.Contains(stderr.String(), `"outcome_unknown":true`) {
		t.Fatal(code, stderr.String())
	}
	code, out, err := mutationInvoke(t, path, "builder", "notes", "add", "--body", "output", "--key", "output-once")
	if code != cli.ExitSuccess || err != "" || !strings.Contains(out, `"revision":1`) {
		t.Fatal(code, out, err)
	}
}

type noteFailWriter struct{}

func (noteFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestNotesCommandsOneFiveSecondBudget(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "notes-budget")
	worker := f.worker(t, project, "Builder")
	var operations atomic.Int64
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
		if strings.HasSuffix(r.URL.Path, "/notes") {
			operations.Add(1)
			<-r.Context().Done()
			return
		}
		t.Error("unrelated request")
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	started := time.Now()
	code, out, err := mutationInvoke(t, path, "builder", "notes", "list")
	elapsed := time.Since(started)
	if code == cli.ExitSuccess || out != "" || !strings.Contains(err, "deadline_exceeded") || elapsed < 4500*time.Millisecond || elapsed > 6*time.Second || operations.Load() != 1 {
		t.Fatal("shared budget/retry", code, out, err, elapsed, operations.Load())
	}
}
