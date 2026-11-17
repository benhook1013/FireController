//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
)

// This qualifies the optional converter and prepared current-job import, not a
// full legacy history migration or an authorized load of an installed project.
func TestPackagedCoordinationImportSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private migration fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		var err error
		binDir, err = filepath.Abs("../../bin")
		if err != nil {
			t.Fatal("binary directory unavailable")
		}
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute packaged binary directory required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binaries, hashes := map[string]string{}, map[string][32]byte{}
	for _, name := range []string{"fc", "fcd", "legacyimport"} {
		path := filepath.Join(binDir, name+suffix)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("packaged %s missing; build the optional migration binaries before migration-smoke", name)
		}
		if _, err := exec.LookPath(path); err != nil {
			t.Fatalf("packaged %s is not executable", name)
		}
		binaries[name] = path
		hashes[name] = binaryHash(t, path)
		t.Logf("packaged migration input %s sha256=%x", name, hashes[name])
	}
	defer func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Errorf("packaged %s input changed", name)
			}
		}
	}()
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	// No HOME/CODEX_HOME overrides or inherited provider credentials. Every
	// application/profile/source path is explicit, and PATH cannot discover tools.
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, other, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	health(t, ctx, daemon.address)
	target, err := url.Parse("http://" + daemon.address)
	if err != nil {
		t.Fatal("daemon origin unavailable")
	}
	var targetMu sync.RWMutex
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }
	var requests, applies, forbidden atomic.Int64
	prefix := "/api/v1/projects/" + project
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == prefix+"/coordination/imports/apply" && r.Method == "POST" {
			applies.Add(1)
		}
		allowed := r.Method == "GET" || r.Method == "POST" && (r.URL.Path == prefix+"/jobs" || r.URL.Path == prefix+"/messages" || r.URL.Path == prefix+"/coordination/imports/preview" || r.URL.Path == prefix+"/coordination/imports/apply") || r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, prefix+"/jobs/")
		if !allowed {
			forbidden.Add(1)
			w.WriteHeader(403)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := filepath.Join(private, "import-admin.json")
	calls, utilityCalls := 0, 0
	var maxCLI time.Duration
	call := func(args ...string) json.RawMessage {
		t.Helper()
		begin := time.Now()
		body := runCLI(t, ctx, binaries["fc"], env, append([]string{"--profiles-file", profiles, "--profile", "import-admin"}, args...)...)
		calls++
		if elapsed := time.Since(begin); elapsed > maxCLI {
			maxCLI = elapsed
		}
		if bytes.Contains(body, bytes.TrimSpace(admin)) {
			t.Fatal("CLI disclosed credential")
		}
		return body
	}
	utility := func(args ...string) json.RawMessage {
		t.Helper()
		body := runCLI(t, ctx, binaries["legacyimport"], env, args...)
		utilityCalls++
		if len(body) > 16<<10 || bytes.Contains(body, bytes.TrimSpace(admin)) {
			t.Fatal("migration utility output exceeded compact safe scope")
		}
		return body
	}
	call("profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
	var unrelated client.JobReceipt
	decode(t, call("jobs", "create", "--title", "Unrelated current job", "--brief", "Keep current brief", "--worker", other, "--key", "unrelated-job"), &unrelated)
	var conversation client.MessageReceipt
	decode(t, call("inbox", "send", "--to", other, "--body", "Existing native conversation", "--key", "unrelated-message"), &conversation)
	unrelatedBefore := call("jobs", "read", unrelated.ID)
	conversationBefore := call("inbox", "thread", conversation.ConversationID)
	inboxBefore := call("inbox", "list")
	initial := readUpgradeState(t, ctx, data)

	snapshot := coordinationLegacySnapshot(t, ctx, private)
	sourceBytes, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal("synthetic source unavailable")
	}
	sourceHash := sha256.Sum256(sourceBytes)
	sourceDigest := hex.EncodeToString(sourceHash[:])
	var inspected struct {
		SHA256 string           `json:"sha256"`
		Counts map[string]int64 `json:"counts"`
		Bytes  int64            `json:"bytes"`
	}
	inspection := utility("inspect", "--snapshot", snapshot)
	decode(t, inspection, &inspected)
	if inspected.SHA256 != sourceDigest || inspected.Bytes != int64(len(sourceBytes)) || inspected.Counts["jobs"] != 2 || inspected.Counts["job_checkpoints"] != 3 || bytes.Contains(inspection, []byte("Original selected brief")) {
		t.Fatal("actual inspector lost source identity or disclosed source text")
	}
	writeJSON := func(name string, value any) string {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal("fixture encoding unavailable")
		}
		path := filepath.Join(private, name)
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("private fixture file unavailable")
		}
		return path
	}
	selection := map[string]any{"version": 1, "repository_id": 991, "expected_revision": 0, "source": "packaged-selected-legacy", "workers": map[string]string{"Old builder": worker}, "job_ids": []string{"source-selected-job"}, "checkpoint_mode": "all"}
	selectionFile := writeJSON("selection.json", selection)
	selectionHash := binaryHash(t, selectionFile)
	prepared := filepath.Join(private, "prepared-jobs.json")
	var preparedSummary struct {
		Kind         string `json:"kind"`
		SourceSHA256 string `json:"source_sha256"`
		Jobs         int    `json:"jobs"`
		Checkpoints  int    `json:"checkpoints"`
	}
	summary := utility("prepare-jobs", "--snapshot", snapshot, "--selection", selectionFile, "--output", prepared)
	decode(t, summary, &preparedSummary)
	if preparedSummary.Kind != "prepared_job_import" || preparedSummary.SourceSHA256 != sourceDigest || preparedSummary.Jobs != 1 || preparedSummary.Checkpoints != 2 || bytes.Contains(summary, []byte("source-selected-job")) {
		t.Fatal("preparation summary changed selection or disclosed source identity")
	}
	preparedInfo, err := os.Lstat(prepared)
	if err != nil || !preparedInfo.Mode().IsRegular() || runtime.GOOS != "windows" && preparedInfo.Mode().Perm()&0077 != 0 {
		t.Fatal("prepared document is not private regular input")
	}
	preparedHash := binaryHash(t, prepared)
	if !bytes.Equal(summary, utility("prepare-jobs", "--snapshot", snapshot, "--selection", selectionFile, "--output", prepared)) {
		t.Fatal("same preparation changed compact result")
	}
	repeatedInfo, err := os.Lstat(prepared)
	if err != nil || !os.SameFile(preparedInfo, repeatedInfo) || binaryHash(t, prepared) != preparedHash {
		t.Fatal("same preparation replaced original file")
	}
	preparedBytes, err := os.ReadFile(prepared)
	if err != nil {
		t.Fatal("prepared document unavailable")
	}
	var batch coordimportmodel.Batch
	decode(t, preparedBytes, &batch)
	if batch.SourceSHA256 != sourceDigest || len(batch.Jobs) != 1 || batch.Jobs[0].SourceKey != "source-selected-job" || batch.Jobs[0].SourceRevision != 9 || batch.Jobs[0].WorkerID != worker || batch.Jobs[0].Brief != "Original selected brief\nExact <literal> text" || len(batch.Jobs[0].Checkpoints) != 2 {
		t.Fatal("prepared original selection or source revision changed")
	}
	var state client.CoordinationImportState
	decode(t, call("import", "state"), &state)
	if state.Revision != 0 {
		t.Fatal("fresh import state is not zero")
	}
	var preview client.CoordinationImportPreview
	decode(t, call("import", "preview", "--file", prepared), &preview)
	if preview.Counts.NewJobs != 1 || preview.Counts.Jobs != 1 || preview.Counts.Checkpoints != 2 {
		t.Fatal("preview selected counts incomplete")
	}
	previewState := readUpgradeState(t, ctx, data)
	if !reflect.DeepEqual(initial, previewState) {
		t.Fatal("inspection/preparation/preview changed destination database")
	}
	applyArgs := []string{"import", "apply", "--file", prepared, "--preview", preview.PreviewSHA256, "--key", "original-import"}
	// A real child with a read-only stdout descriptor commits once but returns
	// output_failed. Recovery must retain the same key/body/binding, not resubmit.
	readOnly, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal("lost output fixture unavailable")
	}
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	command := exec.CommandContext(bounded, binaries["fc"], append([]string{"--profiles-file", profiles, "--profile", "import-admin"}, applyArgs...)...)
	command.Env = env
	command.WaitDelay = time.Second
	command.Stdout = readOnly
	stderr := &boundedOutput{}
	command.Stderr = stderr
	begin := time.Now()
	runErr := command.Run()
	stop()
	_ = readOnly.Close()
	calls++
	if elapsed := time.Since(begin); elapsed > maxCLI {
		maxCLI = elapsed
	}
	var exit *exec.ExitError
	var failure struct {
		Error struct {
			Code           string `json:"code"`
			OutcomeUnknown bool   `json:"outcome_unknown"`
		} `json:"error"`
	}
	if !errors.As(runErr, &exit) || exit.ExitCode() != cli.ExitConfig || stderr.len() > 4096 || json.Unmarshal(stderr.bytes(), &failure) != nil || failure.Error.Code != "output_failed" || !failure.Error.OutcomeUnknown || applies.Load() != 1 || bytes.Contains(stderr.bytes(), bytes.TrimSpace(admin)) {
		t.Fatal("actual committed output-loss recovery signal invalid")
	}
	receiptBytes := call(applyArgs...)
	var receipt client.CoordinationImportReceipt
	decode(t, receiptBytes, &receipt)
	if receipt.Outcome != "imported" || receipt.Revision != 1 || receipt.Counts.NewJobs != 1 || len(receipt.Mappings) != 1 || receipt.Mappings[0].SourceKey != "source-selected-job" {
		t.Fatal("actual apply recovery lost original result")
	}
	jobID := receipt.Mappings[0].JobID
	var job client.Job
	decode(t, call("jobs", "read", jobID), &job)
	if job.Brief != batch.Jobs[0].Brief || job.Title != batch.Jobs[0].Title || job.WorkerID != worker || job.Revision != 1 || job.CreatedAt != "2024-02-01T01:02:03.123456789Z" || job.UpdatedAt != "2024-02-07T04:05:06+01:00" {
		t.Fatal("native job lost original facts or fabricated source revisions")
	}
	if job.Progress.Summary != "Second source checkpoint" {
		t.Fatal("default job read lost imported current progress")
	}
	var progress client.Page[client.ProgressHistoryEntry]
	decode(t, call("jobs", "progress-history", jobID), &progress)
	if len(progress.Items) != 2 || progress.HasMore || progress.Items[0].Progress.Summary != "Second source checkpoint" || progress.Items[0].ActorID != "imported-history" || progress.Items[0].CreatedAt != "2024-02-06T05:06:07.8Z" || progress.Items[1].Progress.Summary != "First source checkpoint" || progress.Items[1].CreatedAt != "2024-02-05T05:06:07Z" || len(progress.Items[1].Progress.Evidence) != 1 || progress.Items[1].Progress.Evidence[0] != `{"pr":7,"proof":["one","two"]}` {
		t.Fatal("unified import history lost retained attribution/text/time/evidence")
	}
	call("jobs", "update", jobID, "--expect-revision", "1", "--brief", "Manual native edit survives retry", "--key", "manual-edit")
	daemon.stop(t)
	retained := readUpgradeState(t, ctx, data)
	changed := map[string]bool{"jobs": true, "job_history": true, "job_checkpoints": true, "job_progress": true, "job_progress_history": true, "coordination_import_state": true, "coordination_import_records": true, "coordination_import_batches": true, "events": true, "receipts": true, "sqlite_sequence": true}
	if initial.version != retained.version || !reflect.DeepEqual(initial.metadata, retained.metadata) || len(initial.tables) != len(retained.tables) {
		t.Fatal("import changed schema/metadata")
	}
	for name, hash := range initial.tables {
		if !changed[name] && (retained.tables[name] != hash || retained.counts[name] != initial.counts[name]) {
			t.Fatalf("import changed unrelated native table %s", name)
		}
	}
	if retained.counts["jobs"] != initial.counts["jobs"]+1 || retained.counts["job_history"] != initial.counts["job_history"]+2 || retained.counts["job_checkpoints"] != initial.counts["job_checkpoints"]+2 || retained.counts["coordination_import_records"] != 1 || retained.counts["coordination_import_batches"] != 1 {
		t.Fatal("selected import widened or duplicated native records")
	}
	coordinationProvenance(t, ctx, data, project, jobID, sourceDigest, batch.Jobs[0])
	daemon = startDaemon(t, ctx, binaries["fcd"], data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("restart changed instance")
	}
	next, err := url.Parse("http://" + daemon.address)
	if err != nil {
		t.Fatal("restart origin unavailable")
	}
	targetMu.Lock()
	target = next
	targetMu.Unlock()
	health(t, ctx, daemon.address)
	if !bytes.Equal(receiptBytes, call(applyArgs...)) {
		t.Fatal("exact retry after edit/restart changed original receipt")
	}
	decode(t, call("jobs", "read", jobID), &job)
	if job.Revision != 2 || job.Brief != "Manual native edit survives retry" {
		t.Fatal("historical retry overwrote manual native edit")
	}
	if !bytes.Equal(unrelatedBefore, call("jobs", "read", unrelated.ID)) || !bytes.Equal(conversationBefore, call("inbox", "thread", conversation.ConversationID)) || !bytes.Equal(inboxBefore, call("inbox", "list")) {
		t.Fatal("import changed unrelated coordination reads")
	}
	daemon.stop(t)
	replayed := readUpgradeState(t, ctx, data)
	if !reflect.DeepEqual(retained, replayed) {
		t.Fatal("replay/restart changed retained records")
	}
	if external.Load() != 0 || forbidden.Load() != 0 || applies.Load() != 3 {
		t.Fatal("workload escaped bounded import/coordination scope")
	}
	if binaryHash(t, snapshot) != sourceHash || binaryHash(t, selectionFile) != selectionHash || binaryHash(t, prepared) != preparedHash {
		t.Fatal("source or approved input bytes changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(snapshot + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("source inspection/preparation created a SQLite sidecar")
		}
	}
	t.Logf("packaged selected coordination import passed: total=%s cli_calls=%d utility_calls=%d api_requests=%d max_cli=%s source_jobs=2 imported_jobs=1 imported_checkpoints=2 output_loss=1 exact_replays=2 unrelated_tables_preserved=%d external_http=0 owned_processes=0; trusted setup separate, no full historical timeline or installed migration claim", time.Since(started).Round(time.Millisecond), calls, utilityCalls, requests.Load(), maxCLI.Round(time.Millisecond), len(initial.tables)-len(changed))
}

// Seed only the synthetic sealed source format. Native state and receipts are
// always created by the actual daemon/CLI, except existing trusted seedAccess.
func coordinationLegacySnapshot(t *testing.T, ctx context.Context, private string) string {
	t.Helper()
	schema, err := os.ReadFile("../legacyimport/internal/legacy/testdata/snapshot-schema.sql")
	if err != nil {
		t.Fatal("canonical synthetic source schema unavailable")
	}
	path := filepath.Join(private, "selected snapshot?#.sqlite")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("synthetic source unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("synthetic source transaction unavailable")
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, string(schema)); err != nil {
		t.Fatal("synthetic source schema failed")
	}
	for _, statement := range []string{`INSERT INTO controller_metadata VALUES(1,3,11)`, `INSERT INTO review_state VALUES(1,'{"schema_version":3}')`, `INSERT INTO job_metadata VALUES(1,2,1,3)`, `INSERT INTO inbox_metadata VALUES(1,1)`, `INSERT INTO map_metadata VALUES(1,1,1,3)`, `INSERT INTO review_records_metadata VALUES(1,9,1,3,11)`} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			t.Fatal("synthetic source version seed failed")
		}
	}
	for _, id := range []string{"source-selected-job", "source-unselected-job"} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO jobs VALUES(?,?,?,NULL,?,'blocked',0,9,3,'Excluded source summary','','','[]',NULL,?,?)`, id, id, "Old builder", "Original <literal> source title", "2024-02-01T01:02:03.123456789Z", "2024-02-07T04:05:06+01:00"); err != nil {
			t.Fatal("synthetic current job seed failed")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO job_briefs VALUES(?,3,'2024-02-07T04:05:06+01:00',?)`, id, "Original selected brief\nExact <literal> text"); err != nil {
			t.Fatal("synthetic brief seed failed")
		}
	}
	for i, row := range []struct{ job, at, done string }{{"source-selected-job", "2024-02-05T05:06:07Z", "First source checkpoint"}, {"source-selected-job", "2024-02-06T05:06:07.8Z", "Second source checkpoint"}, {"source-unselected-job", "2024-02-08T05:06:07Z", "Never selected"}} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO job_checkpoints(sequence,job_id,created_at,done,next_steps,blocker,pointers_json) VALUES(?,?,?,?,'Original next','','{"pr":7,"proof":["one","two"]}')`, i+1, row.job, row.at, row.done); err != nil {
			t.Fatal("synthetic source checkpoint seed failed")
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal("synthetic source commit failed")
	}
	if err = db.Close(); err != nil {
		t.Fatal("synthetic source close failed")
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("synthetic source permissions failed")
	}
	return path
}

func coordinationProvenance(t *testing.T, ctx context.Context, data, project, jobID, digest string, want coordimportmodel.PreparedJob) {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(data, "firecontroller.sqlite")), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("private provenance unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var sourceHash, raw string
	if err = db.QueryRowContext(ctx, `SELECT source_sha256,item_json FROM coordination_import_records WHERE project_id=? AND source='packaged-selected-legacy' AND source_key='source-selected-job' AND job_id=?`, project, jobID).Scan(&sourceHash, &raw); err != nil {
		t.Fatal("native source mapping missing")
	}
	var item coordimportmodel.PreparedJob
	decode(t, []byte(raw), &item)
	if sourceHash != digest || !reflect.DeepEqual(item, want) || item.SourceRevision != 9 {
		t.Fatal("native import provenance changed original facts")
	}
	var nativeHistory, events int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_history WHERE project_id=? AND job_id=?`, project, jobID).Scan(&nativeHistory); err != nil {
		t.Fatal("native history unavailable")
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE project_id=? AND event_type='coordination.imported'`, project).Scan(&events); err != nil {
		t.Fatal("import event unavailable")
	}
	if nativeHistory != 2 || events != 1 {
		t.Fatal("source revision/history or import notifications duplicated")
	}
}
