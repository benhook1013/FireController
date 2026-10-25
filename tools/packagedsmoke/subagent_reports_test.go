//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPackagedSubagentReportsSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private fixture unavailable")
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
		t.Logf("subagent input %s sha256=%x", name, hashes[name])
	}
	t.Cleanup(func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Error("packaged input changed", name)
			}
		}
	})
	var external, engine atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	browserDeny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { engine.Add(1); w.WriteHeader(502) }))
	t.Cleanup(browserDeny.Close)
	// Explicit synthetic profiles/credentials only; do not override shared homes.
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	seedSubagentReportHistory(t, ctx, data, project, worker, bootstrap.instance, strings.TrimSpace(string(admin)))
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	target, _ := url.Parse("http://" + daemon.address)
	var targetMu sync.RWMutex
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }
	var reportWrites, forbiddenWrites, sessionWrites atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+project+"/reviews/subagent-reports") {
				reportWrites.Add(1)
			} else if strings.Contains(r.URL.Path, "/browser-session") && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
				sessionWrites.Add(1)
			} else {
				forbiddenWrites.Add(1)
				w.WriteHeader(403)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := filepath.Join(private, "writer.json")
	base := []string{"--profiles-file", profiles, "--profile", "writer"}
	calls := 0
	cli := func(args ...string) json.RawMessage {
		t.Helper()
		calls++
		return runCLI(t, ctx, binaries["fc"], env, append(append([]string{}, base...), args...)...)
	}
	cli("profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", credential)
	profileHash := binaryHash(t, profiles)
	file := func(name string, value any) string {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal("fixture encoding unavailable")
		}
		path := filepath.Join(private, name)
		if os.WriteFile(path, body, 0600) != nil {
			t.Fatal("private fixture input unavailable")
		}
		return path
	}
	input := client.SubagentRecordRequest{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), Reviewer: "Independent Sol", Scope: "Database invariants", StartedAt: "2026-10-15T00:04:00Z", FinishedAt: "2026-10-15T00:05:00Z", Findings: []client.SubagentFindingInput{{Title: "Preserve database invariant", Detail: "Original **database proof** at `source.go:12`.", Severity: "high"}}}
	inputFile := file("review.json", input)
	var recorded, replay client.SubagentReceipt
	decode(t, cli("reviews", "subagent-record", "--file", inputFile, "--key", "record-subagent"), &recorded)
	decode(t, cli("reviews", "subagent-record", "--file", inputFile, "--key", "record-subagent"), &replay)
	if recorded != replay || recorded.State != "recorded" {
		t.Fatal("CLI exact replay changed review")
	}
	var report client.SubagentReport
	decode(t, cli("reviews", "subagent-read", recorded.ID), &report)
	if report.Run.Channel != "subagent" || report.Run.Found != 1 || report.Run.Finalized || len(report.Findings) != 1 {
		t.Fatal("recorded source mismatch")
	}
	command := exec.CommandContext(ctx, binaries["fcmcp"], base...)
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	session, err := mcp.NewClient(&mcp.Implementation{Name: "packaged-subagent-report", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatal("MCP subprocess initialization failed")
	}
	closed := false
	closeMCP := func() {
		if closed {
			return
		}
		closed = true
		if err := session.Close(); err != nil || stderr.len() != 0 || command.ProcessState == nil || !command.ProcessState.Success() {
			t.Error("owned MCP did not join cleanly")
		}
	}
	t.Cleanup(closeMCP)
	mcpCalls := 0
	tool := func(name string, args any) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		mcpCalls++
		value, err := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || value == nil || value.IsError || len(value.Content) != 1 {
			t.Fatalf("actual MCP %s failed", name)
		}
		body, err := json.Marshal(value.StructuredContent)
		text, ok := value.Content[0].(*mcp.TextContent)
		if err != nil || len(body) > 4<<20 || !ok || !jsonEqual(body, []byte(text.Text)) {
			t.Fatal("MCP response mismatch")
		}
		return body
	}
	raw, _ := json.Marshal(input)
	var recordArgs map[string]any
	_ = json.Unmarshal(raw, &recordArgs)
	recordArgs["idempotency_key"] = "record-subagent"
	decode(t, tool("review_subagent_record", recordArgs), &replay)
	if replay != recorded {
		t.Fatal("CLI/MCP same-key replay changed")
	}
	update := map[string]any{"run_id": recorded.ID, "idempotency_key": "decide-subagent", "expected_revision": recorded.Revision, "decisions": []any{map[string]any{"finding": 0, "disposition": "accepted", "reason": "Verified the source invariant"}}, "finalize": true}
	var decided client.SubagentReceipt
	decode(t, tool("review_subagent_update", update), &decided)
	decode(t, tool("review_subagent_update", update), &replay)
	if decided != replay {
		t.Fatal("MCP update replay changed")
	}
	decode(t, tool("review_subagent_read", map[string]any{"run_id": recorded.ID}), &report)
	if !report.Run.Finalized || report.Run.Accepted != 1 || report.Run.Fixed != 0 {
		t.Fatal("MCP adjudication lost source counts")
	}
	closeMCP()
	expected := ""
	fix := client.SubagentUpdateRequest{ExpectedRevision: &decided.Revision, Fixes: []client.SubagentFixInput{{Finding: 0, ExpectedSHA: &expected, FixSHA: strings.Repeat("c", 40), ProofNote: "Immutable focused fix proof"}}}
	var fixed client.SubagentReceipt
	decode(t, cli("reviews", "subagent-update", recorded.ID, "--file", file("fix.json", fix), "--key", "fix-subagent"), &fixed)
	decode(t, cli("reviews", "subagent-read", recorded.ID), &report)
	if report.Run.Fixed != 1 || report.Run.Accepted != 1 || report.Findings[0].FixSHA != strings.Repeat("c", 40) {
		t.Fatal("CLI fix lost frozen counts")
	}
	before := report
	daemon.stop(t)
	daemon = startDaemon(t, ctx, binaries["fcd"], data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("restart changed instance")
	}
	replacement, _ := url.Parse("http://" + daemon.address)
	targetMu.Lock()
	target = replacement
	targetMu.Unlock()
	decode(t, cli("reviews", "subagent-read", recorded.ID), &report)
	if !reflect.DeepEqual(before, report) {
		t.Fatal("restart changed retained report")
	}
	var runs client.ReviewRuns
	decode(t, cli("reviews", "runs", "42", "--limit", "10"), &runs)
	counts := map[string]int{}
	for _, run := range runs.Items {
		counts[run.Channel]++
	}
	if len(runs.Items) != 3 || counts["cli"] != 1 || counts["hosted"] != 1 || counts["subagent"] != 1 {
		t.Fatal("report changed existing channel inventory", counts)
	}
	if reportWrites.Load() != 6 || forbiddenWrites.Load() != 0 || external.Load() != 0 || binaryHash(t, profiles) != profileHash {
		t.Fatal("unexpected write/provider/profile change", reportWrites.Load(), forbiddenWrites.Load(), external.Load())
	}
	runSubagentReportBrowser(t, ctx, private, env, gateway.URL, project, credential, recorded.ID, browserDeny.URL)
	daemon.stop(t)
	if reportWrites.Load() != 6 || sessionWrites.Load() != 1 || forbiddenWrites.Load() != 0 || external.Load() != 0 {
		t.Fatal("browser changed review/provider state")
	}
	t.Logf("PASS cli_calls=%d mcp_calls=%d report_posts=%d browser_session_posts=%d external=%d browser_engine_denied=%d restart_retained=true binaries_profiles_unchanged=true seconds=%.3f", calls, mcpCalls, reportWrites.Load(), sessionWrites.Load(), external.Load(), engine.Load(), time.Since(started).Seconds())
}

func seedSubagentReportHistory(t *testing.T, ctx context.Context, data, project, worker, instance, admin string) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped history setup unavailable")
	}
	defer store.Close()
	if err := reviews.EnsureSchema(ctx, store); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	pull := reviews.PullInput{Number: 42, Title: "Mixed review history", State: "open", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "unknown", Mergeability: "unknown"}
	for i, channel := range []string{"cli", "hosted"} {
		start, finish := "2026-10-15T00:00:00Z", "2026-10-15T00:01:00Z"
		if i == 1 {
			start, finish = "2026-10-15T00:02:00Z", "2026-10-15T00:03:00Z"
		}
		pull.Runs = append(pull.Runs, reviews.RunInput{SourceRunID: "retained-" + channel, Channel: channel, HeadSHA: pull.HeadSHA, Reviewer: "Original controlled source", Scope: "Retained source", Outcome: "completed", Attributable: true, Complete: true, StartedAt: start, FinishedAt: finish, Finalized: true, CoverageLimits: []string{}, Findings: []reviews.Finding{}})
	}
	body, _ := json.Marshal(reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 991, Source: "packaged-subagent-seed", ObservedAt: "2026-10-15T00:03:00Z", Queue: []reviews.PullInput{pull}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/reviews/observations", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+admin)
	request.Header.Set("X-FC-Instance", instance)
	request.Header.Set("X-FC-Worker", worker)
	request.Header.Set("Idempotency-Key", "seed-subagent-history")
	mux := server.New(store)
	reviews.Register(mux, store)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatal("authenticated history seed failed", response.Code)
	}
}

func runSubagentReportBrowser(t *testing.T, ctx context.Context, private string, env []string, origin, project, credential, run, proxy string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required for browser proof")
	}
	node, _ = filepath.Abs(node)
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal("browser cache unavailable")
		}
		browserPath = filepath.Join(cache, "ms-playwright")
	}
	config := map[string]string{"origin": origin, "project": project, "credential_file": credential, "run_id": run, "proxy": proxy, "executable": os.Getenv("FC_BROWSER_EXECUTABLE")}
	raw, _ := json.Marshal(config)
	file := filepath.Join(private, "browser.json")
	if os.WriteFile(file, raw, 0600) != nil {
		t.Fatal("private browser config unavailable")
	}
	script, _ := filepath.Abs("subagent_reports.mjs")
	command := exec.Command(node, script, file)
	command.Env = append(append([]string{}, env...), "PLAYWRIGHT_BROWSERS_PATH="+browserPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = time.Second
	out, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = out, stderr
	if command.Start() != nil {
		t.Fatal("browser driver unavailable")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	joined := false
	defer func() {
		if pid := out.browserPID(); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if !joined {
			select {
			case <-done:
			case <-time.After(stepTimeout):
				t.Error("browser driver did not join")
			}
		}
	}()
	select {
	case err = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal("browser proof exceeded bound")
	}
	if err != nil || stderr.len() != 0 {
		t.Fatalf("browser render proof failed (stderr_bytes=%d)", stderr.len())
	}
	lines := bytes.Split(bytes.TrimSpace(out.bytes()), []byte("\n"))
	var result struct {
		State    string `json:"state"`
		Errors   int    `json:"errors"`
		External int    `json:"external"`
		Writes   int    `json:"writes"`
		Rows     int    `json:"rows"`
	}
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || result.Errors != 0 || result.External != 0 || result.Writes != 0 || result.Rows != 3 {
		t.Fatal("browser safe render result invalid")
	}
}
