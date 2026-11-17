package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/presentation"
)

func presentationCLIService(t *testing.T) *serviceFixture {
	t.Helper()
	f := setupServiceFixture(t)
	if e := presentation.EnsureSchema(t.Context(), f.store); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	presentation.Register(mux, f.store)
	f.handler = mux
	return f
}
func presentationCLIFile(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "presentation.json")
	if e := os.WriteFile(file, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return file
}
func TestPresentationCLIActualAPIHistoryReplayAndZeroConflict(t *testing.T) {
	f := presentationCLIService(t)
	p, other := f.project(t, "presentation-cli"), f.project(t, "other-presentation")
	worker, otherWorker := f.worker(t, p, "Worker"), f.worker(t, other, "Other")
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/presentation") {
			if r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
				t.Error("binding")
			}
			if r.Method == "GET" {
				reads.Add(1)
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}, "other": {other, otherWorker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "chosen", args...)
		if code != 0 || err != "" {
			t.Fatal(code, out, err)
		}
		return out
	}
	before := mapLedgerCounts(t, f)
	var initial client.PresentationSnapshot
	if json.Unmarshal([]byte(invoke("presentation", "read")), &initial) != nil || initial.Presentation.Revision != 0 || initial.EventCursor == "" || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal(initial)
	}
	file := presentationCLIFile(t, `{"name":"Named <literal>","headings":{"jobs":"Tasks"}}`)
	code, out, diagnostic := mutationInvoke(t, path, "chosen", "presentation", "replace", "--file", file, "--expect-revision", "1", "--key", "stale-zero")
	if code != cli.ExitConflict || out != "" || !strings.Contains(diagnostic, `"current_revision":0`) || !strings.Contains(diagnostic, "project_presentation_revision_conflict") {
		t.Fatal("zero conflict", code, out, diagnostic)
	}
	args := []string{"presentation", "replace", "--file", file, "--expect-revision", "0", "--key", "first"}
	original := invoke(args...)
	var receipt client.PresentationReceipt
	if json.Unmarshal([]byte(original), &receipt) != nil || receipt.Revision != 1 || receipt.Outcome != "updated" || receipt.ProjectID != p || len(original) > 512 || strings.Contains(original, "literal") {
		t.Fatal(original)
	}
	later := presentationCLIFile(t, `{"name":"Later"}`)
	invoke("presentation", "replace", "--file", later, "--expect-revision", "1", "--key", "later")
	before = mapLedgerCounts(t, f)
	if invoke(args...) != original || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("historical replay changed effects")
	}
	var state client.PresentationSnapshot
	if json.Unmarshal([]byte(invoke("presentation", "read")), &state) != nil || state.Presentation.Name != "Later" || state.Presentation.Revision != 2 {
		t.Fatal(state)
	}
	priorReads := reads.Load()
	var page client.Page[client.PresentationHistorySummary]
	if json.Unmarshal([]byte(invoke("presentation", "history", "--limit", "1")), &page) != nil || len(page.Items) != 1 || page.Items[0].Revision != 2 || !page.HasMore || reads.Load() != priorReads+1 {
		t.Fatal("history autopaged", page)
	}
	var old client.PresentationRevision
	if json.Unmarshal([]byte(invoke("presentation", "revision", "1")), &old) != nil || old.State.Name != "Named <literal>" || old.State.Headings["jobs"] != "Tasks" {
		t.Fatal(old)
	}
	_, otherOut, otherErr := mutationInvoke(t, path, "other", "presentation", "read")
	if otherErr != "" || strings.Contains(otherOut, "Later") || !strings.Contains(otherOut, `"revision":0`) {
		t.Fatal("scope leaked", otherOut, otherErr)
	}
	if f.resolverHit.Load() != 2 {
		t.Fatal("provider work during presentation")
	}
}
func TestPresentationCLIInvalidArgumentsAndPrivateFilesBeforeCredentials(t *testing.T) {
	good := presentationCLIFile(t, `{"name":"Good"}`)
	for _, args := range [][]string{{"read", "extra"}, {"read", "--project", "p"}, {"history", "--limit", "0"}, {"history", "--limit", "101"}, {"history", "--limit", "1", "--limit", "2"}, {"revision", "0"}, {"replace", "--file", "relative", "--key", "k", "--expect-revision", "0"}, {"replace", "--file", good, "--key", "k"}, {"replace", "--file", good, "--key", "k", "--expect-revision", "-1"}, {"replace", "--file", good, "--key", "k", "--expect-revision", "9223372036854775807"}, {"replace", "--file", good, "--key", "k", "--expect-revision", "0", "--worker", "foreign"}, {"replace", "--file", good, "--key", "k", "--key", "again", "--expect-revision", "0"}} {
		code, out, err := run(append([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent", "presentation"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") || strings.Contains(err, "PRIVATE") {
			t.Fatal(code, out, err)
		}
	}
	for _, body := range []string{`{"name":"PRIVATE","name":"again"}`, `{"name":"PRIVATE","Name":"alias"}`, `{"name":"PRIVATE","brand_name":null}`, `{"name":"PRIVATE","theme":null}`, `{"name":"PRIVATE","headings":{"jobs":"\ud800"}}`, `{"name":"PRIVATE","theme":{"canvas_text":"#e8e7e4"}}`, string([]byte{0xff}), strings.Repeat("x", (256<<10)+1)} {
		file := presentationCLIFile(t, body)
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent", "presentation", "replace", "--file", file, "--key", "k", "--expect-revision", "0"}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_presentation_file") || strings.Contains(err, "PRIVATE") {
			t.Fatal(code, out, err)
		}
	}
	// A symlink is not the selected private regular file.
	link := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(good, link); e == nil {
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "presentation", "replace", "--file", link, "--key", "k", "--expect-revision", "0"}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_presentation_file") {
			t.Fatal(code, out, err)
		}
	}
}
func TestPresentationCLIFIFOExternallyBounded(t *testing.T) {
	if file := os.Getenv("FC_PRESENTATION_FIFO_CHILD"); file != "" {
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE", "presentation", "replace", "--file", file, "--key", "k", "--expect-revision", "0"}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_presentation_file") {
			t.Fatal(code, out, err)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix FIFO")
	}
	mkfifo, e := exec.LookPath("mkfifo")
	if e != nil {
		t.Skip("mkfifo unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "presentation.fifo")
	if out, e := exec.CommandContext(ctx, mkfifo, file).CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	child := exec.CommandContext(ctx, exe, "-test.run=^TestPresentationCLIFIFOExternallyBounded$")
	child.Env = append(os.Environ(), "FC_PRESENTATION_FIFO_CHILD="+file)
	if out, e := child.CombinedOutput(); e != nil {
		t.Fatal("FIFO blocked or failed", e, string(out))
	}
}
func TestPresentationCLICommittedMalformedReceiptFailedAndShortOutput(t *testing.T) {
	f := presentationCLIService(t)
	p := f.project(t, "presentation-output")
	worker := f.worker(t, p, "Worker")
	var lose atomic.Bool
	lose.Store(true)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/presentation") && lose.CompareAndSwap(true, false) {
			record := httptest.NewRecorder()
			f.handler.ServeHTTP(record, r)
			if record.Code != 200 {
				t.Errorf("not committed%d", record.Code)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			io.WriteString(w, "{")
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}})
	file := presentationCLIFile(t, `{"name":"Output PRIVATE"}`)
	args := []string{"presentation", "replace", "--file", file, "--expect-revision", "0", "--key", "original"}
	code, out, err := mutationInvoke(t, path, "chosen", args...)
	if code == 0 || out != "" || !strings.Contains(err, `"outcome_unknown":true`) {
		t.Fatal(code, out, err)
	}
	before := mapLedgerCounts(t, f)
	for _, writer := range []io.Writer{failedMutationOutput{}, shortMapOutput{}} {
		var diagnostic bytes.Buffer
		code = cli.RunContext(t.Context(), append([]string{"--profiles-file", path, "--profile", "chosen"}, args...), nil, writer, &diagnostic)
		if code != cli.ExitConfig || !strings.Contains(diagnostic.String(), "output_failed") || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostic.String(), "exact same key") || strings.Contains(diagnostic.String(), "PRIVATE") {
			t.Fatal(code, diagnostic.String())
		}
	}
	code, out, err = mutationInvoke(t, path, "chosen", args...)
	if code != 0 || err != "" || !strings.Contains(out, `"revision":1`) || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("exact recovery changed effects", code, out, err)
	}
}
func TestPresentationCLIRejectsNonPrivateFileBeforeCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode privacy; Windows follows native permission semantics")
	}
	file := presentationCLIFile(t, `{"name":"PRIVATE_SETTINGS"}`)
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent", "presentation", "replace", "--file", file, "--key", "k", "--expect-revision", "0"}, nil)
	if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_presentation_file") || strings.Contains(diagnostic, "PRIVATE") {
		t.Fatal("nonprivate file reached credentials", code, out, diagnostic)
	}
}
func TestPresentationCLIReadCancellationHasNoEffects(t *testing.T) {
	f := presentationCLIService(t)
	p := f.project(t, "presentation-cancel")
	worker := f.worker(t, p, "Worker")
	entered := make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/presentation") {
			close(entered)
			<-r.Context().Done()
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}})
	before := mapLedgerCounts(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out, diagnostic bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "chosen", "presentation", "read"}, nil, &out, &diagnostic)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case code := <-done:
		if code != cli.ExitCancelled || out.Len() != 0 || !strings.Contains(diagnostic.String(), "canceled") {
			t.Fatal(code, out.String(), diagnostic.String())
		}
	case <-time.After(time.Second):
		t.Fatal("read cancellation blocked")
	}
	if !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("cancelled read mutated state")
	}
}
