//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This proves real packaged selection and normal native admission, deliberately
// holding at a trusted Git stub before allowance/review execution. All GitHub
// wire reads terminate at an owned loopback HTTPS fixture through CONNECT.
func TestPackagedRunSelectionSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("controlled POSIX Git/CLI stubs and child CA trust qualified by this Linux fixture only")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private selection fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute packaged binaries required")
	}
	binaries := map[string]string{}
	hashes := map[string][32]byte{}
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		binaries[name] = filepath.Join(binDir, name)
		hashes[name] = binaryHash(t, binaries[name])
		t.Logf("selection input %s sha256=%x", name, hashes[name])
	}
	defer func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Errorf("packaged %s changed", name)
			}
		}
	}()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("fixture authority unavailable")
	}
	providerToken := hex.EncodeToString(secret)
	tokenFile := filepath.Join(private, "provider.token")
	if os.WriteFile(tokenFile, []byte(providerToken+"\n"), 0600) != nil {
		t.Fatal("private provider token unavailable")
	}
	provider := newRunSelectionHTTPSFixture(t, private, providerToken)
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + provider.proxy, "HTTPS_PROXY=" + provider.proxy, "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE=" + provider.ca, "SSL_CERT_DIR=" + private}
	gitLog, cliLog := filepath.Join(private, "git.argv"), filepath.Join(private, "review.argv")
	gitStub, cliStub := filepath.Join(private, "git-stub"), filepath.Join(private, "cli-stub")
	for path, logPath := range map[string]string{gitStub: gitLog, cliStub: cliLog} {
		script := "#!/bin/sh\nprintf 'invocation\\n' >> " + runSelectionShellQuote(logPath) + "\nprintf '%s\\n' \"$@\" >> " + runSelectionShellQuote(logPath) + "\nexit 2\n"
		if os.WriteFile(path, []byte(script), 0700) != nil {
			t.Fatal("trusted executable fixture unavailable")
		}
	}
	catalogPath := filepath.Join(private, "catalog.json")
	catalogJSON, _ := json.Marshal(map[string]any{"version": 1, "capabilities": []any{map[string]any{"id": "selection-smoke", "account": "synthetic-account", "organization": "synthetic-org", "channels": []string{"cli"}, "github_credential_file": tokenFile, "git_executable": gitStub, "cli_executable": cliStub, "authentication_environment": map[string]string{}}}})
	if os.WriteFile(catalogPath, catalogJSON, 0600) != nil {
		t.Fatal("private operator catalog unavailable")
	}
	inputs := map[string][32]byte{}
	for _, path := range []string{tokenFile, catalogPath, gitStub, cliStub, provider.ca} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("fixture input unreadable")
		}
		inputs[path] = sha256.Sum256(body)
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil {
		t.Fatal("bootstrap authority unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, workerFile := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	originalSnapshot := seedRunSelectionConfiguration(t, ctx, data, catalogPath, project, worker, strings.TrimSpace(string(admin)), 0, []int64{2, 3}, true)
	initial := readUpgradeState(t, ctx, data)
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env, "--review-catalog", catalogPath)
	target, _ := url.Parse("http://" + daemon.address)
	var targetMu sync.RWMutex
	forward := httputil.NewSingleHostReverseProxy(target)
	forward.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	forward.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }
	var appHTTP, unexpected atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appHTTP.Add(1)
		if r.Method != "GET" && !(r.Method == "POST" && r.URL.Path == "/api/v1/projects/"+project+"/reviews/selections") {
			unexpected.Add(1)
			w.WriteHeader(403)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := filepath.Join(private, "profiles.json")
	calls := 0
	safe := func(body []byte) {
		t.Helper()
		if len(body) > 64<<10 || bytes.Contains(body, bytes.TrimSpace(admin)) || bytes.Contains(body, []byte(providerToken)) {
			t.Fatal("output exceeded bounds or disclosed credential")
		}
	}
	call := func(args ...string) json.RawMessage {
		t.Helper()
		out := runCLI(t, ctx, binaries["fc"], env, append([]string{"--profiles-file", profiles, "--profile", "selector"}, args...)...)
		calls++
		safe(out)
		return out
	}
	call("profile", "create", "--server", gateway.URL, "--instance", bootstrap.instance, "--project", project, "--worker", worker, "--credential-file", workerFile)
	originalArgs := []string{"reviews", "start", "--channel", "cli", "--expect-pr", "3", "--expected-configuration-revision", "1", "--key", "selected-once", "--wait-seconds", "0"}
	enqueueStarted := time.Now()
	original := call(originalArgs...)
	enqueueDuration := time.Since(enqueueStarted)
	var receipt client.ReviewRequestReceipt
	decode(t, original, &receipt)
	if receipt.State != "queued" || receipt.Revision != 1 || !strings.HasPrefix(receipt.ID, "review_selection_") || len(original) > 256 {
		t.Fatal("selection receipt was not original/compact")
	}
	var selected client.ReviewRunState
	decode(t, call("reviews", "run-read", "--selection-id", receipt.ID, "--wait-seconds", "2"), &selected)
	assertSelected := func(value client.ReviewRunState) {
		t.Helper()
		if value.Selection.ID != receipt.ID || value.Selection.State != "selected" || value.Selection.ExpectedPullNumber != 3 || value.Selection.ProjectID != project || value.Selection.WorkerID != worker || value.Operation == nil || value.Operation.ID != value.Selection.OperationID || value.Operation.PullNumber != 3 || value.Operation.State != "held" || value.Operation.Reason != "source_unavailable" {
			t.Fatal("selected operation did not reach deliberate Git hold")
		}
	}
	assertSelected(selected)
	gitBefore, err := os.ReadFile(gitLog)
	if err != nil || bytes.Count(gitBefore, []byte("invocation\n")) != 1 || bytes.Contains(gitBefore, []byte(providerToken)) {
		t.Fatal("trusted Git fixture was not invoked safely")
	}
	if _, err = os.Stat(cliLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("allowance/review executable unexpectedly invoked")
	}
	providerBefore := provider.inventory()
	expectedFirst := map[string]int{"GET /repositories/991": 2, "GET /repos/synthetic/packaged-smoke/pulls/2": 1, "GET /repos/synthetic/packaged-smoke/pulls/3": 2}
	if !reflect.DeepEqual(providerBefore, expectedFirst) {
		t.Fatalf("first source request inventory differs: %v", providerBefore)
	}
	// Actual packaged provider reads update pre-existing display facts without
	// another provider request, while the original pinned snapshot stays intact.
	for _, number := range []string{"2", "3"} {
		var current client.ReviewPull
		decode(t, call("reviews", "pull", number), &current)
		if current.Pull.Title != "Synthetic selection target" || current.Pull.Diff == nil || *current.Pull.Diff != (client.ReviewPullDiff{Files: 1, Additions: 3, Deletions: 2}) || current.Pull.IsDraft == nil || *current.Pull.IsDraft {
			t.Fatal("fresh source metadata missing from actual CLI read")
		}
	}
	var pinned client.ReviewPull
	decode(t, call("reviews", "pull", "3", "--snapshot", originalSnapshot), &pinned)
	if pinned.Pull.Title != "Before provider" || pinned.Pull.IsDraft == nil || !*pinned.Pull.IsDraft || pinned.Pull.Diff == nil || pinned.Pull.Diff.Files != 17 {
		t.Fatal("source publication rewrote original pinned metadata")
	}
	if !reflect.DeepEqual(provider.inventory(), expectedFirst) {
		t.Fatal("stored metadata reads reacquired provider facts")
	}
	// Expect-PR is a genuine assertion: the closed channel front remains PR3,
	// so another explicit invocation expecting PR2 creates no native child/Git.
	var mismatch client.ReviewRunState
	decode(t, call("reviews", "start", "--channel", "cli", "--expect-pr", "2", "--key", "wrong-front", "--wait-seconds", "2"), &mismatch)
	if mismatch.Selection.State != "held" || mismatch.Selection.Reason != "expected_pr_mismatch" || mismatch.Operation != nil {
		t.Fatal("expected PR redirected selection")
	}
	if body, err := os.ReadFile(gitLog); err != nil || !bytes.Equal(body, gitBefore) {
		t.Fatal("mismatched expected PR touched Git")
	}
	expectedProvider := map[string]int{"GET /repositories/991": 3, "GET /repos/synthetic/packaged-smoke/pulls/2": 2, "GET /repos/synthetic/packaged-smoke/pulls/3": 3}
	if !reflect.DeepEqual(provider.inventory(), expectedProvider) {
		t.Fatalf("bounded source request inventory differs: %v", provider.inventory())
	}
	// Official SDK stdio shares exactly the same binding/key, without acquisition.
	command := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profiles, "--profile", "selector")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "packaged-run-selection", Version: "1"}, nil)
	connectCtx, stopConnect := context.WithTimeout(ctx, stepTimeout)
	session, err := sdk.Connect(connectCtx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stopConnect()
	if err != nil {
		if command.Process != nil {
			command.Process.Kill()
		}
		t.Fatal("actual SDK initialization failed")
	}
	var closeOnce sync.Once
	closeMCP := func() {
		closeOnce.Do(func() {
			if err := session.Close(); err != nil {
				t.Error("MCP close failed")
			}
			if command.ProcessState == nil || !command.ProcessState.Success() || stderr.len() != 0 {
				t.Error("MCP process did not join cleanly")
			}
		})
	}
	t.Cleanup(closeMCP)
	mcpCalls := 0
	tool := func(name string, args any) json.RawMessage {
		t.Helper()
		toolCtx, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		result, err := session.CallTool(toolCtx, &mcp.CallToolParams{Name: name, Arguments: args})
		mcpCalls++
		if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
			t.Fatal("actual SDK selection tool failed")
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal("SDK structured output unavailable")
		}
		safe(raw)
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok || !jsonEqual(raw, []byte(text.Text)) {
			t.Fatal("SDK compatibility text differs")
		}
		return raw
	}
	var mcpResult client.ReviewRunState
	decode(t, tool("review_start", map[string]any{"channel": "cli", "expected_pull_number": 3, "expected_configuration_revision": 1, "idempotency_key": "selected-once", "wait_seconds": 2}), &mcpResult)
	assertSelected(mcpResult)
	decode(t, tool("review_selection_read", map[string]any{"selection_id": receipt.ID}), &mcpResult)
	assertSelected(mcpResult)
	if !reflect.DeepEqual(provider.inventory(), expectedProvider) {
		t.Fatal("MCP replay/read acquired source")
	}
	// Output failure is uncertain, but recovering the original key cannot spend
	// another review. No replacement action or private profile is invented.
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal("stdout failure fixture unavailable")
	}
	lostCtx, stopLost := context.WithTimeout(ctx, stepTimeout)
	lost := exec.CommandContext(lostCtx, binaries["fc"], append([]string{"--profiles-file", profiles, "--profile", "selector"}, originalArgs...)...)
	lost.Env = env
	lost.Stdout = null
	lostErr := &boundedOutput{}
	lost.Stderr = lostErr
	lost.WaitDelay = time.Second
	lostRun := lost.Run()
	stopLost()
	null.Close()
	calls++
	safe(lostErr.bytes())
	var exit *exec.ExitError
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Unknown bool   `json:"outcome_unknown"`
		} `json:"error"`
	}
	if !errors.As(lostRun, &exit) || exit.ExitCode() != cli.ExitConfig || json.Unmarshal(lostErr.bytes(), &failure) != nil || failure.Error.Code != "output_failed" || !failure.Error.Unknown {
		t.Fatal("lost selection output was not recoverably uncertain")
	}
	if !bytes.Equal(original, call(originalArgs...)) {
		t.Fatal("wait0 recovery changed original receipt")
	}
	closeMCP()
	daemon.stop(t)
	beforeRestart := readUpgradeState(t, ctx, data)
	seedRunSelectionConfiguration(t, ctx, data, catalogPath, project, worker, strings.TrimSpace(string(admin)), 1, []int64{3, 2}, false)
	afterConfig := readUpgradeState(t, ctx, data)
	restarted := startDaemon(t, ctx, binaries["fcd"], data, env, "--review-catalog", catalogPath)
	if restarted.instance != bootstrap.instance {
		t.Fatal("restart changed instance")
	}
	next, _ := url.Parse("http://" + restarted.address)
	targetMu.Lock()
	target = next
	targetMu.Unlock()
	replayArgs := append([]string(nil), originalArgs...)
	replayArgs[len(replayArgs)-1] = "2"
	var replay client.ReviewRunState
	decode(t, call(replayArgs...), &replay)
	assertSelected(replay)
	decode(t, call("reviews", "run-read", "--selection-id", receipt.ID), &replay)
	assertSelected(replay)
	if !reflect.DeepEqual(provider.inventory(), expectedProvider) {
		t.Fatal("later config/restart/replay/read reacquired source")
	}
	if body, err := os.ReadFile(gitLog); err != nil || !bytes.Equal(body, gitBefore) {
		t.Fatal("restart or replay launched another Git command")
	}
	restarted.stop(t)
	final := readUpgradeState(t, ctx, data)
	if !reflect.DeepEqual(final, afterConfig) {
		t.Fatal("restart/recovery/read altered retained tables")
	}
	changed := map[string]bool{"review_selections": true, "review_operations": true, "review_execution_attempts": true, "review_execution_transitions": true, "review_execution_reservations": true, "events": true, "receipts": true, "sqlite_sequence": true}
	for _, name := range []string{"review_snapshots", "review_workspace", "review_current_pulls", "review_pull_versions", "review_pull_memberships", "review_current_attempts", "review_attempt_versions", "review_attempt_memberships", "review_current_totals", "review_current_attempt_order"} {
		changed[name] = true
	}
	for name, h := range initial.tables {
		if !changed[name] && (beforeRestart.tables[name] != h || beforeRestart.counts[name] != initial.counts[name]) {
			t.Fatalf("selection changed unrelated table %s", name)
		}
	}
	if beforeRestart.counts["review_selections"] != 2 || beforeRestart.counts["review_operations"] != 1 || beforeRestart.counts["review_execution_attempts"] != 1 || beforeRestart.counts["review_execution_reservations"] != 0 {
		t.Fatal("selection duplicated work or retained false reservation")
	}
	for _, name := range []string{"review_execution_evidence", "review_adjudication_state", "review_round_completions"} {
		if beforeRestart.counts[name] != 0 {
			t.Fatalf("selection fabricated captured/finalized credit in %s", name)
		}
	}
	for path, h := range inputs {
		body, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(body) != h {
			t.Fatal("private immutable fixture input changed")
		}
	}
	if unexpected.Load() != 0 || provider.forbidden.Load() != 0 {
		t.Fatal("fixture observed unexpected method/route/origin")
	}
	t.Logf("packaged one-call selection passed: total=%s enqueue=%s cli_calls=%d mcp_calls=%d application_http=%d loopback_github_gets=8 git_stub_calls=1 cli_review_calls=0 native_operations=1 owned_processes=0", time.Since(started).Round(time.Millisecond), enqueueDuration.Round(time.Millisecond), calls, mcpCalls, appHTTP.Load())
}

func runSelectionShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
func seedRunSelectionConfiguration(t *testing.T, ctx context.Context, data, catalogPath, project, worker, admin string, previous int64, queue []int64, initial bool) string {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped selection setup unavailable")
	}
	defer store.Close()
	catalog, err := reviewruntime.LoadCatalog(catalogPath)
	if err != nil || len(catalog.Capabilities) != 1 {
		t.Fatal("trusted catalog invalid")
	}
	cap := catalog.Capabilities[0]
	engine, err := reviewexec.New(store, []reviewexec.Capability{{ID: cap.ID, Provider: "coderabbit", Account: cap.Account, Channels: cap.Channels, Fingerprint: cap.Fingerprint}})
	if err != nil {
		t.Fatal("trusted setup engine invalid")
	}
	mux := http.NewServeMux()
	engine.Register(mux)
	reviews.Register(mux, store)
	var originalSnapshot string
	call := func(path, key string, body any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal("setup body unavailable")
		}
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-FC-Instance", store.InstanceID())
		r.Header.Set("X-FC-Worker", worker)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code < 200 || w.Code >= 300 {
			t.Fatalf("trusted configuration rejected status=%d", w.Code)
		}
		if strings.HasSuffix(path, "/observations") {
			var receipt reviews.Receipt
			decode(t, w.Body.Bytes(), &receipt)
			originalSnapshot = receipt.ID
		}
	}
	prefix := "/api/v1/projects/" + project + "/reviews"
	if initial {
		call(prefix+"/policy", "policy", map[string]any{"expected_revision": 0, "required_channels": []string{"cli"}, "paused_channels": []string{}, "max_review_files": 0})
	}
	call(prefix+"/execution/configuration", fmt.Sprintf("configuration-%d", previous), map[string]any{"expected_revision": previous, "capability_id": cap.ID, "mode": "independent", "queue": queue, "channels": []string{"cli"}, "paused": false})
	if initial {
		call(prefix+"/rounds/2/cli", "close-two", map[string]any{"expected_revision": 0, "action": "close"})
		pulls := []reviews.PullInput{}
		for _, n := range []int64{2, 3} {
			draft := true
			pulls = append(pulls, reviews.PullInput{Number: n, Title: "Before provider", State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", CIState: "unknown", Mergeability: "unknown", Diff: &reviews.PullDiff{Files: 17, Additions: 1, Deletions: 1}, IsDraft: &draft})
		}
		zero := int64(0)
		call(prefix+"/observations", "initial-source-metadata", reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 991, Source: "synthetic-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: pulls})
	}
	return originalSnapshot
}

type runSelectionHTTPSFixture struct {
	proxy, ca string
	mu        sync.Mutex
	paths     map[string]int
	forbidden atomic.Int64
}

func (f *runSelectionHTTPSFixture) inventory() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.paths {
		out[k] = v
	}
	return out
}
func newRunSelectionHTTPSFixture(t *testing.T, private, token string) *runSelectionHTTPSFixture {
	t.Helper()
	f := &runSelectionHTTPSFixture{paths: map[string]int{}}
	cert, ca := historyCollectionCertificate(t)
	f.ca = filepath.Join(private, "fixture-ca.pem")
	if os.WriteFile(f.ca, ca, 0600) != nil {
		t.Fatal("private CA unavailable")
	}
	repo := map[string]any{"id": 991, "full_name": "synthetic/packaged-smoke", "default_branch": "main"}
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths[r.Method+" "+r.URL.Path]++
		f.mu.Unlock()
		refuse := func() { f.forbidden.Add(1); w.WriteHeader(502) }
		if r.Method != "GET" || r.Host != "api.github.com" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.ContentLength > 0 {
			refuse()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/repositories/991":
			body = repo
		case "/repos/synthetic/packaged-smoke/pulls/2", "/repos/synthetic/packaged-smoke/pulls/3":
			number := 2
			if strings.HasSuffix(r.URL.Path, "/3") {
				number = 3
			}
			body = map[string]any{"number": number, "title": "Synthetic selection target", "state": "open", "merged": false, "head": map[string]any{"sha": strings.Repeat("a", 40), "repo": repo}, "base": map[string]any{"sha": strings.Repeat("b", 40), "ref": "main", "repo": repo}, "changed_files": 1, "additions": 3, "deletions": 2, "draft": false}
		default:
			refuse()
			return
		}
		if json.NewEncoder(w).Encode(body) != nil {
			f.forbidden.Add(1)
		}
	}))
	api.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	api.Config.ErrorLog = log.New(io.Discard, "", 0)
	api.StartTLS()
	t.Cleanup(func() { api.CloseClientConnections(); api.Close() })
	var tunnels sync.Map
	var wg sync.WaitGroup
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Add(1)
		defer wg.Done()
		if r.Method != "CONNECT" || r.Host != "api.github.com:443" {
			f.forbidden.Add(1)
			w.WriteHeader(502)
			return
		}
		upstream, err := net.DialTimeout("tcp", api.Listener.Addr().String(), stepTimeout)
		if err != nil {
			f.forbidden.Add(1)
			w.WriteHeader(502)
			return
		}
		downstream, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			f.forbidden.Add(1)
			return
		}
		tunnels.Store(downstream, upstream)
		defer func() { downstream.Close(); upstream.Close(); tunnels.Delete(downstream) }()
		if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if buffer.Flush() != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() { io.Copy(upstream, buffer); done <- struct{}{} }()
		go func() { io.Copy(downstream, upstream); done <- struct{}{} }()
		<-done
		downstream.Close()
		upstream.Close()
		<-done
	}))
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.proxy = proxy.URL
	t.Cleanup(func() {
		tunnels.Range(func(k, v any) bool { k.(net.Conn).Close(); v.(net.Conn).Close(); return true })
		proxy.CloseClientConnections()
		proxy.Close()
		joined := make(chan struct{})
		go func() { wg.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(stepTimeout):
			t.Error("owned selection CONNECT handlers did not join")
		}
	})
	return f
}
