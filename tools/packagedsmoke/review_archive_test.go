//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stopped fixture deliberately exceeds observation admission's old ceiling.
// This proves bounded archive reads, not cap-free publication or migration.
func TestPackagedReviewArchiveSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private archive fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute packaged binary directory required")
	}
	binaries, hashes := map[string]string{}, map[string][32]byte{}
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		binaries[name] = filepath.Join(binDir, name)
		hashes[name] = binaryHash(t, binaries[name])
	}
	// Reuse the existing private proof inventory; do not rebuild or rewrite
	// source/assets while executing the selected packaged inputs.
	sourceInputs := map[string]map[string][32]byte{}
	for _, directory := range []string{"../../cmd", "../../internal", "../../webapp/src"} {
		sourceInputs[directory] = upgradeFileHashes(t, directory)
	}
	archiveDriverHash := binaryHash(t, "review_archive.mjs")
	defer func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Errorf("packaged archive input %s changed", name)
			}
		}
		for directory, before := range sourceInputs {
			if !reflect.DeepEqual(before, upgradeFileHashes(t, directory)) {
				t.Errorf("archive smoke changed source/embedded assets in %s", directory)
			}
		}
		if binaryHash(t, "review_archive.mjs") != archiveDriverHash {
			t.Error("archive browser driver changed during packaged proof")
		}
	}()
	var external, engine atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	browserDeny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { engine.Add(1); w.WriteHeader(502) }))
	t.Cleanup(browserDeny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("archive bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, other, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	snapshot, firstRun, tailRun, newer := seedPackagedReviewArchive(t, ctx, data, project, worker, bootstrap.instance, strings.TrimSpace(string(admin)))
	newObservationFile := filepath.Join(private, "new-observation.json")
	if os.WriteFile(newObservationFile, newer, 0600) != nil {
		t.Fatal("private newer observation unavailable")
	}
	before := readUpgradeState(t, ctx, data)
	oldSnapshot := packagedArchiveSnapshotHash(t, ctx, data, project, snapshot)
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("archive daemon identity changed")
	}
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	var requests, writes, observations atomic.Int64
	prefix := "/api/v1/projects/" + project
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			writes.Add(1)
			if r.Method == http.MethodPost && r.URL.Path == prefix+"/reviews/observations" {
				observations.Add(1)
			} else if !(r.URL.Path == prefix+"/browser-session" && (r.Method == http.MethodPost || r.Method == http.MethodDelete) || r.URL.Path == prefix+"/browser-session/renew" && r.Method == http.MethodPost) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profileFile := filepath.Join(private, "archive-profile.json")
	runCLI(t, ctx, binaries["fc"], env, "--profiles-file", profileFile, "--profile", "archive", "profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", credential)
	cli := func(args ...string) json.RawMessage {
		t.Helper()
		previous := requests.Load()
		out := runCLI(t, ctx, binaries["fc"], env, append([]string{"--profiles-file", profileFile, "--profile", "archive", "reviews"}, args...)...)
		if requests.Load() != previous+1 || bytes.Contains(out, bytes.TrimSpace(admin)) {
			t.Fatal("archive CLI silently paged, made extra calls or disclosed authority")
		}
		return out
	}
	type archiveItem struct {
		ID              string `json:"id"`
		Source          string `json:"source"`
		SourceRunID     string `json:"source_run_id"`
		SourceAttemptID string `json:"source_attempt_id"`
	}
	type archivePage struct {
		SnapshotID string        `json:"snapshot_id"`
		Source     string        `json:"source"`
		Items      []archiveItem `json:"items"`
		Cursor     string        `json:"cursor"`
		HasMore    bool          `json:"has_more"`
	}
	checkPage := func(raw []byte, first int, run bool) archivePage {
		t.Helper()
		var page archivePage
		decode(t, raw, &page)
		if len(page.Items) != 5 || page.SnapshotID != snapshot || page.Source != "archive-publisher" || !page.HasMore || page.Cursor == "" {
			t.Fatal("packaged archive page lost default-five bound, snapshot or publisher")
		}
		for i, item := range page.Items {
			want := fmt.Sprintf("archive-attempt-%03d", first-i)
			got := item.SourceAttemptID
			if run {
				want, got = fmt.Sprintf("archive-run-%03d", first-i), item.SourceRunID
			}
			if got != want || item.Source != "producer-a" {
				t.Fatal("packaged archive order or original producer changed")
			}
		}
		return page
	}
	for _, kind := range []string{"runs", "attempts"} {
		first := checkPage(cli(kind, "123", "--snapshot", snapshot), 122, kind == "runs")
		checkPage(cli(kind, "123", "--snapshot", snapshot, "--cursor", first.Cursor), 117, kind == "runs")
	}
	var pull map[string]json.RawMessage
	decode(t, cli("pull", "123", "--snapshot", snapshot), &pull)
	if _, exists := pull["attempts"]; exists {
		t.Fatal("compact PR detail expanded attempts")
	}

	command := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profileFile, "--profile", "archive")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	connectCtx, stopConnect := context.WithTimeout(ctx, stepTimeout)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "packaged-archive", Version: "1"}, nil).Connect(connectCtx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stopConnect()
	if err != nil {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Fatal("packaged archive MCP unavailable")
	}
	closed := false
	closeMCP := func() {
		if closed {
			return
		}
		closed = true
		done := make(chan error, 1)
		go func() { done <- session.Close() }()
		select {
		case err := <-done:
			if err != nil || stderr.len() != 0 {
				t.Error("archive MCP failed clean shutdown")
			}
		case <-time.After(stepTimeout):
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("archive MCP did not join")
			}
			t.Error("archive MCP shutdown exceeded budget")
		}
	}
	t.Cleanup(closeMCP)
	callTool := func(name string, args map[string]any) []byte {
		t.Helper()
		previous := requests.Load()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		value, err := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || value == nil || value.IsError || len(value.Content) != 1 || requests.Load() != previous+1 {
			t.Fatal("archive MCP silently paged or failed")
		}
		body, err := json.Marshal(value.StructuredContent)
		text, ok := value.Content[0].(*mcp.TextContent)
		if err != nil || !ok || len(body) > 64<<10 || !jsonEqual(body, []byte(text.Text)) || bytes.Contains(body, bytes.TrimSpace(admin)) {
			t.Fatal("archive MCP structured output is not bounded matching JSON")
		}
		return body
	}
	for _, kind := range []string{"runs", "attempts"} {
		first := checkPage(callTool("review_"+kind, map[string]any{"pull_number": 123, "snapshot": snapshot}), 122, kind == "runs")
		checkPage(callTool("review_"+kind, map[string]any{"pull_number": 123, "snapshot": snapshot, "cursor": first.Cursor}), 117, kind == "runs")
	}
	closeMCP()
	if writes.Load() != 0 || !reflect.DeepEqual(before, readUpgradeState(t, ctx, data)) || oldSnapshot != packagedArchiveSnapshotHash(t, ctx, data, project, snapshot) {
		t.Fatal("packaged CLI/MCP archive reads changed durable state")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required for archive browser smoke")
	}
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal("browser cache unavailable")
		}
		browserPath = filepath.Join(cache, "ms-playwright")
	}
	if !filepath.IsAbs(browserPath) {
		t.Fatal("absolute browser cache required")
	}
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if executable != "" && !filepath.IsAbs(executable) {
		t.Fatal("absolute browser executable required")
	}
	config := map[string]string{"origin": gateway.URL, "project": project, "worker": worker, "other_worker": other, "instance": daemon.instance, "admin_file": adminFile, "credential_file": credential, "executable": executable, "proxy": browserDeny.URL, "snapshot": snapshot, "pull_number": "123", "count": "123", "first_attempt_id": "archive-attempt-122", "tail_attempt_id": "shared-attempt", "first_run_id": firstRun, "tail_run_id": tailRun, "source_a": "producer-a", "source_b": "producer-b", "new_observation_file": newObservationFile}
	raw, _ := json.Marshal(config)
	configFile := filepath.Join(private, "archive-browser.json")
	if os.WriteFile(configFile, raw, 0600) != nil {
		t.Fatal("private archive browser configuration unavailable")
	}
	script, _ := filepath.Abs("review_archive.mjs")
	browser := exec.Command(node, script, configFile)
	browser.Env = append(env, "PLAYWRIGHT_BROWSERS_PATH="+browserPath)
	browser.SysProcAttr, browser.WaitDelay = &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, browserStderr := &browserOutput{}, &boundedOutput{}
	browser.Stdout, browser.Stderr = output, browserStderr
	if browser.Start() != nil {
		t.Fatal("archive browser could not start")
	}
	done := make(chan error, 1)
	go func() { done <- browser.Wait() }()
	joined := false
	defer func() {
		if pid := output.browserPID(); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-browser.Process.Pid, syscall.SIGKILL)
		if !joined {
			select {
			case <-done:
			case <-time.After(stepTimeout):
				t.Error("archive browser did not join")
			}
		}
	}()
	select {
	case err = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal("archive browser exceeded bounded workload")
	}
	var result struct {
		State            string `json:"state"`
		Phase            string `json:"phase"`
		ArchiveRequests  int    `json:"archive_requests"`
		MaxLimit         int    `json:"max_limit"`
		SourceCollisions bool   `json:"source_collisions"`
		PinnedTail       bool   `json:"pinned_tail"`
		CurrentReset     bool   `json:"current_reset"`
		FixtureWrites    int    `json:"fixture_writes"`
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	if len(lines) == 2 {
		_ = json.Unmarshal(lines[1], &result)
	}
	if err != nil || browserStderr.len() != 0 || len(lines) != 2 || result.State != "PASS" || result.MaxLimit != 20 || result.ArchiveRequests != 20 || !result.SourceCollisions || !result.PinnedTail || !result.CurrentReset || result.FixtureWrites != 1 {
		phase := "unavailable"
		switch result.Phase {
		case "configuration", "launch", "current-first-pages", "pinned-first-pages", "metadata-refresh", "pinned-continuation":
			phase = result.Phase
		}
		t.Fatalf("packaged archive browser failed at phase=%s; raw output withheld to protect fixture authority", phase)
	}
	daemon.stop(t)
	after := readUpgradeState(t, ctx, data)
	changed := map[string]int{"review_snapshots": 1, "review_pulls": 1, "review_runs": 1, "review_attempts": 1, "events": 1, "receipts": 1, "review_workspace": 0, "sqlite_sequence": 0}
	if len(before.tables) != len(after.tables) || !reflect.DeepEqual(before.metadata, after.metadata) || oldSnapshot != packagedArchiveSnapshotHash(t, ctx, data, project, snapshot) {
		t.Fatal("archive browser changed schema, metadata or retained snapshot bytes")
	}
	for name, hash := range before.tables {
		if delta, deliberate := changed[name]; deliberate {
			if after.counts[name] != before.counts[name]+delta {
				t.Fatalf("unexpected deliberate fixture row delta in %s", name)
			}
		} else if after.tables[name] != hash || after.counts[name] != before.counts[name] {
			t.Fatalf("archive browser changed unrelated durable table %s", name)
		}
	}
	if external.Load() != 0 || observations.Load() != 1 {
		t.Fatal("archive workload attempted provider/external HTTP")
	}
	t.Logf("packaged archives passed in %s: runs=123 attempts=123 CLI/MCP default5+explicit continuation, browser max20 pinned-tail/current-reset/source-collision, fixture_observations=1 daemon_external=0 blocked_engine=%d owned_processes=0", time.Since(started).Round(time.Millisecond), engine.Load())
}

func seedPackagedReviewArchive(t *testing.T, ctx context.Context, data, project, worker, instance, admin string) (string, string, string, []byte) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped archive setup unavailable")
	}
	defer store.Close()
	zero := int64(0)
	at := time.Now().UTC().Add(-time.Hour)
	pull := reviews.PullInput{Number: 123, Title: "Retained archive metadata", State: "open", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "unknown", Mergeability: "unknown", Complete: true}
	observation := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 991, Source: "archive-publisher", ObservedAt: at.Format(time.RFC3339Nano), Queue: []reviews.PullInput{pull}, Retired: []reviews.RetiredPull{}}
	raw, _ := json.Marshal(observation)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/reviews/observations", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+admin)
	r.Header.Set("X-FC-Instance", instance)
	r.Header.Set("X-FC-Worker", worker)
	r.Header.Set("Idempotency-Key", "packaged-archive-initial")
	w := httptest.NewRecorder()
	server.New(store).ServeHTTP(w, r)
	var receipt reviews.Receipt
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" || receipt.Revision != 1 {
		t.Fatal("authenticated initial archive observation failed")
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("stopped synthetic archive transaction unavailable")
	}
	defer tx.Rollback()
	var recent []reviews.RunSummary
	var latest reviews.Attempt
	var first, tail string
	for i := 0; i < 123; i++ {
		source, attemptID := "producer-a", fmt.Sprintf("archive-attempt-%03d", i)
		if i < 2 {
			attemptID = "shared-attempt"
			if i == 1 {
				source = "producer-b"
			}
		}
		runSourceID := fmt.Sprintf("archive-run-%03d", i)
		digest := sha256.Sum256([]byte(project + "\x00" + source + "\x00" + runSourceID))
		id := "review_run_" + hex.EncodeToString(digest[:])[:32]
		start, finish := at.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), at.Add(time.Duration(i+1)*time.Second).Format(time.RFC3339Nano)
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_runs(project_id,snapshot_id,snapshot_revision,pull_number,run_order,id,source,source_run_id,channel,head_sha,reviewer,scope,outcome,attributable,complete,started_at,finished_at,found,accepted,routed,finalized,fixed,coverage_limits_json) VALUES(?,?,1,123,?,?,?,?,'cli',?,'Synthetic reviewer','full','completed',1,1,?,?,0,0,0,1,0,'[]')`, project, receipt.ID, i, id, source, runSourceID, pull.HeadSHA, start, finish); err != nil {
			t.Fatal("synthetic retained run seed failed")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_attempts(project_id,snapshot_id,pull_number,position,source,source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id) VALUES(?,?,123,?,?,?,'cli',?,'completed',?,?,1,?,'','')`, project, receipt.ID, i, source, attemptID, pull.HeadSHA, start, finish, runSourceID); err != nil {
			t.Fatal("synthetic retained attempt seed failed")
		}
		if i == 0 {
			tail = id
		}
		if i == 122 {
			first = id
		}
		if i >= 120 {
			recent = append([]reviews.RunSummary{{Source: source, ID: id, PullNumber: 123, SourceRunID: runSourceID, Channel: "cli", HeadSHA: pull.HeadSHA, Reviewer: "Synthetic reviewer", Scope: "full", Outcome: "completed", Attributable: true, Complete: true, StartedAt: start, FinishedAt: finish, Finalized: true, CoverageLimits: []string{}}}, recent...)
		}
		one := int64(1)
		latest = reviews.Attempt{SourceAttemptID: attemptID, Channel: "cli", HeadSHA: pull.HeadSHA, State: "completed", StartedAt: start, FinishedAt: finish, DurationSeconds: &one, SourceRunID: runSourceID}
	}
	recentJSON, _ := json.Marshal(recent)
	latestJSON, _ := json.Marshal([]reviews.AttemptSummary{reviews.AttemptSummary(latest)})
	if _, err = tx.ExecContext(ctx, `UPDATE review_pulls SET run_count=123,attempt_count=123,recent_runs_json=?,latest_attempts_json=? WHERE project_id=? AND snapshot_id=? AND number=123`, string(recentJSON), string(latestJSON), project, receipt.ID); err != nil || tx.Commit() != nil {
		t.Fatal("synthetic compact archive projection failed")
	}
	one := int64(1)
	pull.Title = "Current archive metadata"
	pull.Runs = []reviews.RunInput{{SourceRunID: "current-only-run", Channel: "cli", HeadSHA: pull.HeadSHA, Reviewer: "Synthetic reviewer", Scope: "full", Outcome: "completed", Attributable: true, Complete: true, StartedAt: at.Add(3 * time.Minute).Format(time.RFC3339Nano), FinishedAt: at.Add(4 * time.Minute).Format(time.RFC3339Nano), Finalized: true, CoverageLimits: []string{}, Findings: []reviews.Finding{}}}
	pull.Attempts = []reviews.Attempt{{SourceAttemptID: "current-only-attempt", Channel: "cli", HeadSHA: pull.HeadSHA, State: "completed", StartedAt: at.Add(3 * time.Minute).Format(time.RFC3339Nano), FinishedAt: at.Add(4 * time.Minute).Format(time.RFC3339Nano), DurationSeconds: &one, SourceRunID: "current-only-run"}}
	observation.ExpectedRevision, observation.ObservedAt, observation.Queue = &one, at.Add(5*time.Minute).Format(time.RFC3339Nano), []reviews.PullInput{pull}
	newer, _ := json.Marshal(observation)
	return receipt.ID, first, tail, newer
}

func packagedArchiveSnapshotHash(t *testing.T, ctx context.Context, data, project, snapshot string) [32]byte {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.Join(data, "firecontroller.sqlite"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("read-only archive snapshot unavailable")
	}
	defer db.Close()
	hash := sha256.New()
	for _, table := range []string{"review_snapshots", "review_pulls", "review_runs", "review_attempts", "review_findings", "review_retired_pulls"} {
		rows, err := db.QueryContext(ctx, `SELECT * FROM `+table+` WHERE project_id=? AND snapshot_id=? ORDER BY rowid`, project, snapshot)
		if err != nil {
			t.Fatal("retained archive bytes unavailable")
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal("retained archive columns unavailable")
		}
		for rows.Next() {
			values, ptrs := make([]any, len(cols)), make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if rows.Scan(ptrs...) != nil || json.NewEncoder(hash).Encode(values) != nil {
				rows.Close()
				t.Fatal("retained archive rows unavailable")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal("retained archive read interrupted")
		}
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}
