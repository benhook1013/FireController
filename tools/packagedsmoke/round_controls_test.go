//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Actual operator transports configure settings; this workload deliberately
// dispatches no provider and cannot manufacture historical completed rounds.
func TestPackagedRoundControlsSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private round fixture unavailable")
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
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		path := filepath.Join(binDir, name+suffix)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("packaged %s missing; run make build", name)
		}
		if _, err = exec.LookPath(path); err != nil {
			t.Fatalf("packaged %s not executable", name)
		}
		binaries[name], hashes[name] = path, binaryHash(t, path)
		t.Logf("round input %s sha256=%x", name, hashes[name])
	}
	defer func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Errorf("packaged %s changed", name)
			}
		}
	}()
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	// Explicit paths only: neither installed profiles/provider secrets nor HOME
	// overrides enter these children. PATH cannot find a provider executable.
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	otherProject, otherWorker, readerFile := seedRoundAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), project, private)
	reader, err := os.ReadFile(readerFile)
	if err != nil {
		t.Fatal("reader credential unavailable")
	}
	adminFile := filepath.Join(private, "round-admin.token")
	if os.WriteFile(adminFile, admin, 0600) != nil {
		t.Fatal("private admin copy unavailable")
	}
	initial := readUpgradeState(t, ctx, data)
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	target, _ := url.Parse("http://" + daemon.address)
	var targetMu sync.RWMutex
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }
	var requests, posts, unexpected atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodPost {
			posts.Add(1)
			if !strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+project+"/reviews/rounds/") && !strings.HasPrefix(r.URL.Path, "/api/v1/projects/"+otherProject+"/reviews/rounds/") {
				unexpected.Add(1)
				w.WriteHeader(403)
				return
			}
		} else if r.Method != http.MethodGet {
			unexpected.Add(1)
			w.WriteHeader(403)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := map[string]string{"admin": filepath.Join(private, "round-admin.json"), "other": filepath.Join(private, "round-other.json"), "reader": filepath.Join(private, "round-reader.json")}
	calls := 0
	var maxCLI time.Duration
	safe := func(body []byte) {
		t.Helper()
		if bytes.Contains(body, bytes.TrimSpace(admin)) || bytes.Contains(body, bytes.TrimSpace(reader)) {
			t.Fatal("transport exposed credential")
		}
	}
	call := func(name string, args ...string) json.RawMessage {
		t.Helper()
		defer func() {
			if t.Failed() {
				t.Logf("round CLI failure at invocation=%d", calls+1)
			}
		}()
		begin := time.Now()
		body := runCLI(t, ctx, binaries["fc"], env, append([]string{"--profiles-file", profiles[name], "--profile", name}, args...)...)
		calls++
		if elapsed := time.Since(begin); elapsed > maxCLI {
			maxCLI = elapsed
		}
		safe(body)
		return body
	}
	for _, binding := range []struct{ name, project, worker, credential string }{{"admin", project, worker, adminFile}, {"other", otherProject, otherWorker, adminFile}, {"reader", project, worker, readerFile}} {
		call(binding.name, "profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", binding.project, "--worker", binding.worker, "--credential-file", binding.credential)
	}
	profileHashes := map[string][32]byte{}
	for name, path := range profiles {
		profileHashes[name] = binaryHash(t, path)
	}
	readArgs := func(pull, channel string) []string {
		return []string{"reviews", "rounds", "read", "--pr", pull, "--channel", channel}
	}
	read := func(name, pull, channel string) client.ReviewRoundControl {
		t.Helper()
		var c client.ReviewRoundControl
		decode(t, call(name, readArgs(pull, channel)...), &c)
		return c
	}
	if c := read("admin", "42", "cli"); c != (client.ReviewRoundControl{ProjectID: project, PullNumber: 42, Channel: "cli"}) {
		t.Fatal("unconfigured scope not explicit")
	}
	grant := []string{"reviews", "rounds", "grant", "--pr", "42", "--channel", "cli", "--expected-revision", "0", "--max", "5", "--key", "round-original"}
	original := call("admin", grant...)
	if c := read("reader", "42", "cli"); !c.Configured || c.Minimum != 0 || c.Maximum != 5 || c.Taper != 3 || c.Used != 0 || c.Dry != 0 || c.Epoch != 1 || c.Revision != 1 {
		t.Fatal("CLI default additional grant incorrect")
	}
	exact := []string{"reviews", "rounds", "grant", "--pr", "43", "--channel", "cli", "--expected-revision", "0", "--exact", "2", "--key", "round-exact"}
	call("admin", exact...)
	if c := read("admin", "43", "cli"); c.Minimum != 2 || c.Maximum != 2 || c.Used != 0 {
		t.Fatal("CLI exact grant incorrect")
	}
	command := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profiles["admin"], "--profile", "admin")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "packaged-rounds", Version: "1"}, nil)
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	session, err := sdk.Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stop()
	if err != nil {
		if command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Fatal("actual MCP initialization failed")
	}
	var closeOnce sync.Once
	done := make(chan struct{})
	var closeErr error
	closeMCP := func() {
		closeOnce.Do(func() { go func() { closeErr = session.Close(); close(done) }() })
		select {
		case <-done:
			if closeErr != nil || command.ProcessState == nil || !command.ProcessState.Success() || stderr.len() != 0 {
				t.Error("owned MCP shutdown failed")
			}
		case <-time.After(stepTimeout):
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("owned MCP did not join")
			}
			t.Error("MCP shutdown exceeded bound")
		}
	}
	t.Cleanup(closeMCP)
	mcpCalls := 0
	tool := func(name string, args any, wantError string) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		r, err := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		mcpCalls++
		if err != nil || r == nil || len(r.Content) != 1 || r.IsError != (wantError != "") {
			t.Fatalf("actual MCP tool %s failed", name)
		}
		body, err := json.Marshal(r.StructuredContent)
		if err != nil || len(body) > 4096 {
			t.Fatal("MCP compact output invalid")
		}
		safe(body)
		text, ok := r.Content[0].(*mcp.TextContent)
		if !ok || !jsonEqual(body, []byte(text.Text)) {
			t.Fatal("MCP compatibility text diverged")
		}
		if wantError != "" {
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			decode(t, body, &failure)
			if failure.Error.Code != wantError {
				t.Fatal("MCP error classification incorrect")
			}
		}
		return body
	}
	hosted := map[string]any{"pull_number": 42, "channel": "hosted", "expected_revision": 0, "action": "grant", "minimum": 0, "maximum": 2, "idempotency_key": "round-hosted"}
	tool("review_rounds_configure", hosted, "")
	closeArgs := map[string]any{"pull_number": 42, "channel": "cli", "expected_revision": 1, "action": "close", "idempotency_key": "round-close"}
	closed := tool("review_rounds_configure", closeArgs, "")
	cliClose := []string{"reviews", "rounds", "close", "--pr", "42", "--channel", "cli", "--expected-revision", "1", "--key", "round-close"}
	if !jsonEqual(closed, call("admin", cliClose...)) || !jsonEqual(closed, tool("review_rounds_configure", closeArgs, "")) {
		t.Fatal("cross-transport close replay changed receipt")
	}
	var mcpControl client.ReviewRoundControl
	decode(t, tool("review_rounds_read", map[string]any{"pull_number": 42, "channel": "cli"}, ""), &mcpControl)
	if !mcpControl.Closed || mcpControl.Revision != 2 || mcpControl.Used != 0 || mcpControl.Dry != 0 || mcpControl.Tapered {
		t.Fatal("MCP scoped closed state incorrect")
	}
	if c := read("admin", "42", "hosted"); c.Closed || c.Maximum != 2 || c.Taper != 1 {
		t.Fatal("CLI closure crossed channel")
	}
	if c := read("admin", "43", "cli"); c.Closed || c.Minimum != 2 {
		t.Fatal("CLI closure crossed PR")
	}
	if c := read("other", "42", "cli"); c.Configured || c.ProjectID != otherProject {
		t.Fatal("MCP closure crossed project")
	}
	call("other", "reviews", "rounds", "grant", "--pr", "42", "--channel", "cli", "--expected-revision", "0", "--max", "7", "--key", "round-other")
	if c := read("other", "42", "cli"); c.Maximum != 7 || c.Closed || c.Used != 0 {
		t.Fatal("second profile lost explicit project binding")
	}
	// Same MCP session must reread credentials before every tool, including
	// replay of an existing successful admin receipt. Readers retain read access.
	beforeDenied := readUpgradeState(t, ctx, data)
	if os.WriteFile(adminFile, reader, 0600) != nil {
		t.Fatal("reauthorization fixture unavailable")
	}
	tool("review_rounds_read", map[string]any{"pull_number": 42, "channel": "cli"}, "")
	tool("review_rounds_configure", closeArgs, "forbidden")
	deniedCtx, stopDenied := context.WithTimeout(ctx, stepTimeout)
	denied := exec.CommandContext(deniedCtx, binaries["fc"], append([]string{"--profiles-file", profiles["reader"], "--profile", "reader"}, cliClose...)...)
	denied.Env = env
	denied.WaitDelay = time.Second
	deniedOut, deniedErr := &boundedOutput{}, &boundedOutput{}
	denied.Stdout, denied.Stderr = deniedOut, deniedErr
	deniedRun := denied.Run()
	stopDenied()
	calls++
	var exit *exec.ExitError
	var failure struct {
		Error struct {
			Code           string `json:"code"`
			OutcomeUnknown bool   `json:"outcome_unknown"`
		} `json:"error"`
	}
	if !errors.As(deniedRun, &exit) || exit.ExitCode() == 0 || deniedOut.len() != 0 || deniedErr.len() > 4096 || json.Unmarshal(deniedErr.bytes(), &failure) != nil || failure.Error.Code != "forbidden" || failure.Error.OutcomeUnknown {
		t.Fatal("read-only CLI replay was not safely denied")
	}
	safe(deniedErr.bytes())
	if !reflect.DeepEqual(beforeDenied, readUpgradeState(t, ctx, data)) {
		t.Fatal("denied control changed durable state")
	}
	if os.WriteFile(adminFile, admin, 0600) != nil {
		t.Fatal("admin fixture restoration failed")
	}
	if !jsonEqual(closed, tool("review_rounds_configure", closeArgs, "")) {
		t.Fatal("same-session authority recovery changed old receipt")
	}
	// Commit with an unwritable stdout, then retain the original key across a
	// later configuration change and actual process restart.
	reopen := []string{"reviews", "rounds", "grant", "--pr", "42", "--channel", "cli", "--expected-revision", "2", "--max", "4", "--key", "round-lost-output"}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal("lost output fixture unavailable")
	}
	lostCtx, stopLost := context.WithTimeout(ctx, stepTimeout)
	lost := exec.CommandContext(lostCtx, binaries["fc"], append([]string{"--profiles-file", profiles["admin"], "--profile", "admin"}, reopen...)...)
	lost.Env = env
	lost.WaitDelay = time.Second
	lost.Stdout = null
	lostErr := &boundedOutput{}
	lost.Stderr = lostErr
	lostRun := lost.Run()
	stopLost()
	null.Close()
	calls++
	failure.Error.Code = ""
	failure.Error.OutcomeUnknown = false
	if !errors.As(lostRun, &exit) || exit.ExitCode() != cli.ExitConfig || lostErr.len() > 4096 || json.Unmarshal(lostErr.bytes(), &failure) != nil || failure.Error.Code != "output_failed" || !failure.Error.OutcomeUnknown {
		t.Fatal("committed lost stdout did not report unknown outcome")
	}
	safe(lostErr.bytes())
	recovered := call("admin", reopen...)
	var reopened client.ReviewRoundControlReceipt
	decode(t, recovered, &reopened)
	if reopened.Revision != 3 || reopened.Epoch != 2 || reopened.Outcome != "granted" {
		t.Fatal("lost stdout recovery receipt incorrect")
	}
	call("admin", "reviews", "rounds", "close", "--pr", "42", "--channel", "cli", "--expected-revision", "3", "--key", "round-later-close")
	beforeRestart := read("admin", "42", "cli")
	if beforeRestart.Revision != 4 || beforeRestart.Epoch != 2 || !beforeRestart.Closed || beforeRestart.Used != 0 {
		t.Fatal("later change did not preserve new empty allowance")
	}
	closeMCP()
	daemon.stop(t)
	retained := readUpgradeState(t, ctx, data)
	changed := map[string]bool{"review_round_controls": true, "review_round_changes": true, "events": true, "receipts": true, "sqlite_sequence": true}
	if retained.version != initial.version || !reflect.DeepEqual(retained.metadata, initial.metadata) || len(retained.tables) != len(initial.tables) {
		t.Fatal("round configuration changed schema or identity")
	}
	for name, hash := range initial.tables {
		if !changed[name] && (retained.tables[name] != hash || retained.counts[name] != initial.counts[name]) {
			t.Fatalf("round settings changed unrelated table %s", name)
		}
	}
	for _, name := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_round_operation_bindings", "review_round_completions"} {
		if count, exists := retained.counts[name]; !exists || count != 0 {
			t.Fatalf("operator settings fabricated native work in %s", name)
		}
	}
	restarted := startDaemon(t, ctx, binaries["fcd"], data, env)
	if restarted.instance != bootstrap.instance {
		t.Fatal("restart changed instance")
	}
	next, _ := url.Parse("http://" + restarted.address)
	targetMu.Lock()
	target = next
	targetMu.Unlock()
	if !bytes.Equal(recovered, call("admin", reopen...)) || !bytes.Equal(original, call("admin", grant...)) || !jsonEqual(closed, call("admin", cliClose...)) || read("admin", "42", "cli") != beforeRestart {
		t.Fatal("restart lost settings or original cross-epoch receipts")
	}
	restarted.stop(t)
	if !reflect.DeepEqual(retained, readUpgradeState(t, ctx, data)) {
		t.Fatal("restart/replay changed retained rows")
	}
	for name, path := range profiles {
		if profileHashes[name] != binaryHash(t, path) {
			t.Fatal("bound profile changed")
		}
	}
	if !bytes.Equal(admin, mustRoundFile(t, adminFile)) || !bytes.Equal(reader, mustRoundFile(t, readerFile)) {
		t.Fatal("bound profile or restored credential bytes changed")
	}
	if external.Load() != 0 || unexpected.Load() != 0 {
		t.Fatal("round settings attempted external or unsupported work")
	}
	t.Logf("packaged round controls passed: total=%s cli_calls=%d max_cli=%s mcp_calls=%d application_http=%d control_posts=%d external_http=0 provider_operations=0 owned_processes=0", time.Since(started).Round(time.Millisecond), calls, maxCLI.Round(time.Millisecond), mcpCalls, requests.Load(), posts.Load())
}

func mustRoundFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private fixture input unavailable")
	}
	return body
}

// Additional stopped, authenticated setup: one separate project+worker and an
// actual read-only grant. No SQL constructs review settings or completion facts.
func seedRoundAccess(t *testing.T, ctx context.Context, data, instance, admin, project, private string) (string, string, string) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("trusted round setup unavailable")
	}
	defer store.Close()
	if store.InstanceID() != instance {
		t.Fatal("trusted round setup identity changed")
	}
	mux := server.New(store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 992, FullName: "synthetic/round-other"}, nil
	}))
	call := func(path string, body any) string {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal("trusted setup JSON invalid")
		}
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-FC-Instance", instance)
		r.Header.Set("Idempotency-Key", core.NewID("round-setup"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var receipt struct {
			ID string `json:"id"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" {
			t.Fatal("trusted round setup failed")
		}
		return receipt.ID
	}
	other := call("/api/v1/projects", map[string]any{"name": "Round isolation", "repository": "synthetic/round-other"})
	worker := call("/api/v1/projects/"+other+"/workers", map[string]any{"name": "Builder"})
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal("private reader credential unavailable")
	}
	token := hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	call("/api/v1/projects/"+project+"/credentials", map[string]any{"token_sha256": hex.EncodeToString(hash[:]), "permissions": []string{"read"}})
	path := filepath.Join(private, "round-reader.token")
	if os.WriteFile(path, []byte(token+"\n"), 0600) != nil {
		t.Fatal("private reader file unavailable")
	}
	return other, worker, path
}
