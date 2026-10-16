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
	"github.com/benhook1013/FireController/internal/projectmap"
)

func mapCLIService(t *testing.T) *serviceFixture {
	t.Helper()
	f := setupServiceFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	f.handler = mux
	return f
}
func mapCLIFile(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "entry.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestProjectMapCommandsActualAPIHistoryReferencesAndReplay(t *testing.T) {
	f := mapCLIService(t)
	p, other := f.project(t, "map-cli"), f.project(t, "map-cli-other")
	worker, otherWorker := f.worker(t, p, "Builder"), f.worker(t, other, "Builder")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}, "other": {other, otherWorker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, path, "chosen", args...)
		if code != 0 || err != "" {
			t.Fatalf("map CLI %d %s %s", code, out, err)
		}
		return out
	}
	content := map[string]any{"kind": "workstream", "title": "  Area  ", "description": "PRIVATE_MAP <script>literal</script>", "phases": []any{map[string]any{"id": "first", "name": "  First  "}}, "links": []any{map[string]any{"label": " Spec ", "url": "https://example.test/spec"}}}
	file := mapCLIFile(t, content)
	createArgs := []string{"map", "create", "--file", file, "--key", "map-create"}
	var original client.ProjectMapReceipt
	raw := invoke(createArgs...)
	if json.Unmarshal([]byte(raw), &original) != nil || original.Revision != 1 || original.Outcome != "created" || strings.Contains(raw, "PRIVATE_MAP") || len(raw) > 512 {
		t.Fatal(raw)
	}
	var current client.ProjectMapEntry
	if json.Unmarshal([]byte(invoke("map", "read", original.ID)), &current) != nil || current.ProjectID != p || current.Title != "Area" || current.Phases[0].Name != "First" || current.Description != content["description"] || current.Archived {
		t.Fatal(current)
	}
	returnFile := mapCLIFile(t, map[string]any{"kind": "return_point", "title": "Later", "trigger": "when ready"})
	invoke("map", "create", "--file", returnFile, "--key", "map-return")
	var page client.Page[client.ProjectMapSummary]
	raw = invoke("map", "list", "--limit", "1")
	if json.Unmarshal([]byte(raw), &page) != nil || len(page.Items) != 1 || !page.HasMore || strings.Contains(raw, "PRIVATE_MAP") || strings.Contains(raw, "description") {
		t.Fatal(raw)
	}
	if json.Unmarshal([]byte(invoke("map", "list", "--limit", "1", "--cursor", page.Cursor)), &page) != nil || len(page.Items) != 1 || page.HasMore {
		t.Fatal(page)
	}
	if json.Unmarshal([]byte(invoke("map", "list", "--kind", "return_point", "--archived", "false")), &page) != nil || len(page.Items) != 1 || page.Items[0].Kind != "return_point" {
		t.Fatal(page)
	}
	job := mutationSuccess(t, path, "chosen", "jobs", "create", "--title", "Task", "--brief", "BRIEF_UNCHANGED", "--key", "map-job")["id"].(string)
	var association client.JobWorkstream
	if json.Unmarshal([]byte(invoke("jobs", "workstream", job)), &association) != nil || association.Revision != 0 || association.WorkstreamID != "" {
		t.Fatal(association)
	}
	code, out, diagnostic := mutationInvoke(t, path, "chosen", "jobs", "workstream-set", job, "--workstream", original.ID, "--expect-revision", "1", "--key", "stale-zero")
	if code != cli.ExitConflict || out != "" || !strings.Contains(diagnostic, `"current_revision":0`) || !strings.Contains(diagnostic, "job_workstream_revision_conflict") {
		t.Fatal(code, out, diagnostic)
	}
	setArgs := []string{"jobs", "workstream-set", job, "--workstream", original.ID, "--expect-revision", "0", "--key", "set-once"}
	setRaw := invoke(setArgs...)
	content["title"] = "Renamed"
	content["phases"].([]any)[0].(map[string]any)["name"] = "Renamed phase"
	renamed := mapCLIFile(t, content)
	replaceArgs := []string{"map", "replace", original.ID, "--file", renamed, "--expect-revision", "1", "--key", "rename-once"}
	var changed client.ProjectMapReceipt
	changedRaw := invoke(replaceArgs...)
	if json.Unmarshal([]byte(changedRaw), &changed) != nil || changed.Revision != 2 {
		t.Fatal(changedRaw)
	}
	noop := invoke("map", "replace", original.ID, "--file", renamed, "--expect-revision", "2", "--key", "rename-noop")
	if !strings.Contains(noop, `"outcome":"unchanged"`) {
		t.Fatal(noop)
	}
	if json.Unmarshal([]byte(invoke("jobs", "workstream", job)), &association) != nil || association.WorkstreamTitle != "Area" || association.Revision != 1 {
		t.Fatal(association)
	}
	var refs client.Page[client.JobWorkstreamHistoryEntry]
	if json.Unmarshal([]byte(invoke("jobs", "workstream-history", job, "--limit", "1")), &refs) != nil || len(refs.Items) != 1 || refs.Items[0].WorkstreamTitle != "Area" || refs.Items[0].JobRevision != 1 {
		t.Fatal(refs)
	}
	content["archived"] = true
	archived := mapCLIFile(t, content)
	invoke("map", "replace", original.ID, "--file", archived, "--expect-revision", "2", "--key", "archive")
	if invoke(createArgs...) != rawReceipt(t, original) || invoke(replaceArgs...) != changedRaw {
		t.Fatal("entry receipts changed after later replacement")
	}
	if json.Unmarshal([]byte(invoke("map", "list", "--kind", "workstream", "--archived", "true")), &page) != nil || len(page.Items) != 1 || !page.Items[0].Archived {
		t.Fatal(page)
	}
	var history client.Page[client.ProjectMapHistorySummary]
	raw = invoke("map", "history", original.ID, "--limit", "1")
	if json.Unmarshal([]byte(raw), &history) != nil || len(history.Items) != 1 || history.Items[0].Revision != 3 || !history.HasMore || strings.Contains(raw, "PRIVATE_MAP") {
		t.Fatal(raw)
	}
	if json.Unmarshal([]byte(invoke("map", "history", original.ID, "--limit", "1", "--cursor", history.Cursor)), &history) != nil || history.Items[0].Revision != 2 || !history.HasMore {
		t.Fatal(history)
	}
	var revision client.ProjectMapRevision
	if json.Unmarshal([]byte(invoke("map", "revision", original.ID, "1")), &revision) != nil || revision.State.Title != "Area" || revision.State.Revision != 1 || revision.State.Description != content["description"] {
		t.Fatal(revision)
	}
	invoke("jobs", "workstream-set", job, "--clear", "--expect-revision", "1", "--key", "clear-once")
	before := mapLedgerCounts(t, f)
	if invoke(setArgs...) != setRaw {
		t.Fatal("old association replay changed")
	}
	invoke("jobs", "workstream", job)
	invoke("map", "read", original.ID)
	invoke("jobs", "workstream-history", job)
	if !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("reads/replay changed ledger")
	}
	var jobState client.Job
	if json.Unmarshal([]byte(invoke("jobs", "read", job)), &jobState) != nil || jobState.Revision != 1 || jobState.Brief != "BRIEF_UNCHANGED" || jobState.Status != "active" {
		t.Fatal(jobState)
	}
	code, out, diagnostic = mutationInvoke(t, path, "other", "map", "read", original.ID)
	if code == 0 || out != "" || !strings.Contains(diagnostic, "not_found") || strings.Contains(diagnostic, "PRIVATE_MAP") {
		t.Fatal("foreign map exposed", code, out, diagnostic)
	}
	if f.resolverHit.Load() != 2 {
		t.Fatal("map commands used provider", f.resolverHit.Load())
	}
}
func rawReceipt(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}
func mapLedgerCounts(t *testing.T, f *serviceFixture) []int {
	t.Helper()
	var result []int
	for _, table := range []string{"events", "receipts"} {
		var n int
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		result = append(result, n)
	}
	return result
}
func TestProjectMapCommandsRejectFilesAndArgumentsBeforeCredentials(t *testing.T) {
	dir := t.TempDir()
	valid := mapCLIFile(t, map[string]any{"kind": "workstream", "title": "Good"})
	for _, body := range [][]byte{[]byte(`{"kind":"workstream","title":"PRIVATE","Title":"alias"}`), []byte(`{"kind":"workstream","title":"PRIVATE","title":"again"}`), []byte(`{"kind":"workstream","title":null}`), []byte(`{"kind":"return_point","title":"PRIVATE","where":"wrong"}`), []byte(`{"kind":"workstream","title":"PRIVATE","links":[{"label":"bad","url":"javascript:alert(1)"}]}`), []byte(`{"kind":"workstream","title":"\ud800"}`), []byte{0xff}, []byte(strings.Repeat("x", (64<<10)+1))} {
		file := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent", "map", "create", "--file", file, "--key", "k"}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_map_file") || strings.Contains(err, "PRIVATE") {
			t.Fatal("file validation used credentials or leaked content", code, out, err)
		}
	}
	for _, args := range [][]string{
		{"map", "create", "--file", "relative", "--key", "k"}, {"map", "create", "--file", valid}, {"map", "create", "--file", valid, "--key", "k", "--key", "k"}, {"map", "create", "--file", valid, "--key", "k", "--worker", "other"}, {"map", "replace", "id", "--file", valid, "--key", "k", "--expect-revision", "0"}, {"map", "replace", "id", "--file", valid, "--key", "k", "--expect-revision", "9223372036854775807"}, {"map", "list", "--kind", "phase"}, {"map", "list", "--archived", "TRUE"}, {"map", "list", "--archived"}, {"map", "list", "--limit", "0"}, {"map", "list", "--cursor", " padded"}, {"map", "read", "../foreign"}, {"map", "revision", "id", "0"}, {"map", "history", "id", "--limit", "101"},
		{"jobs", "workstream-set", "job", "--expect-revision", "0", "--key", "k"}, {"jobs", "workstream-set", "job", "--clear", "--workstream", "id", "--expect-revision", "0", "--key", "k"}, {"jobs", "workstream-set", "job", "--clear=false", "--expect-revision", "0", "--key", "k"}, {"jobs", "workstream-set", "job", "--clear", "--expect-revision", "-1", "--key", "k"}, {"jobs", "workstream", "job", "--worker", "other"},
	} {
		code, out, err := run(append([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") || strings.Contains(err, "PRIVATE") {
			t.Fatal("argument validation used credentials", code, out, err)
		}
	}
}
func TestProjectMapFIFOIsExternallyBounded(t *testing.T) {
	if file := os.Getenv("FC_MAP_FIFO_CHILD"); file != "" {
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent", "map", "create", "--file", file, "--key", "k"}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_map_file") {
			t.Fatal(code, out, err)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix FIFO fixture")
	}
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	file := filepath.Join(t.TempDir(), "entry.fifo")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, mkfifo, file).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, executable, "-test.run=^TestProjectMapFIFOIsExternallyBounded$")
	child.Env = append(os.Environ(), "FC_MAP_FIFO_CHILD="+file)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatal("FIFO rejection failed or blocked", err, string(out))
	}
}

type shortMapOutput struct{}

func (shortMapOutput) Write(value []byte) (int, error) { return len(value) - 1, nil }

func TestProjectMapCommittedOutputFailureAndSharedCancellation(t *testing.T) {
	f := mapCLIService(t)
	p := f.project(t, "map-output")
	worker := f.worker(t, p, "Builder")
	var corrupt atomic.Bool
	corrupt.Store(true)
	entered := make(chan struct{})
	var block atomic.Bool
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() && r.Method == "GET" && strings.Contains(r.URL.Path, "/map/entries/") {
			close(entered)
			<-r.Context().Done()
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/map/entries") && corrupt.CompareAndSwap(true, false) {
			record := httptest.NewRecorder()
			f.handler.ServeHTTP(record, r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(record.Code)
			_, _ = io.WriteString(w, "{")
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}})
	file := mapCLIFile(t, map[string]any{"kind": "workstream", "title": "Output"})
	args := []string{"map", "create", "--file", file, "--key", "same-key"}
	code, out, err := mutationInvoke(t, path, "chosen", args...)
	if code == 0 || out != "" || !strings.Contains(err, `"outcome_unknown":true`) {
		t.Fatal(code, out, err)
	}
	before := mapLedgerCounts(t, f)
	var stderr bytes.Buffer
	for _, output := range []io.Writer{failedMutationOutput{}, shortMapOutput{}} {
		stderr.Reset()
		code = cli.RunContext(t.Context(), append([]string{"--profiles-file", path, "--profile", "chosen"}, args...), nil, output, &stderr)
		if code != cli.ExitConfig || !strings.Contains(stderr.String(), "output_failed") || !strings.Contains(stderr.String(), `"outcome_unknown":true`) {
			t.Fatal(code, stderr.String())
		}
	}
	var receipt client.ProjectMapReceipt
	code, out, err = mutationInvoke(t, path, "chosen", args...)
	if code != 0 || err != "" || json.Unmarshal([]byte(out), &receipt) != nil || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("same-key output recovery changed effects", code, out, err)
	}
	block.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	var stdout bytes.Buffer
	stderr.Reset()
	go func() {
		done <- cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "chosen", "map", "read", receipt.ID}, nil, &stdout, &stderr)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("map read did not start")
	}
	cancel()
	select {
	case code = <-done:
	case <-time.After(time.Second):
		t.Fatal("map read cancellation blocked")
	}
	if code != cli.ExitCancelled || stdout.Len() != 0 || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("cancel changed map or returned data", code, stdout.String(), stderr.String())
	}
}
