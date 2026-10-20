//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exact request intent crosses the actual three binaries and a real durable
// operation. Source reads terminate at the existing loopback HTTPS fixture;
// the trusted Git stub holds both operations before any review executable.
func TestPackagedExplicitOrderSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("controlled POSIX executable and child CA trust fixture is Linux-only")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private explicit-order fixture unavailable")
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
		t.Logf("explicit order input %s sha256=%x", name, hashes[name])
	}
	defer func() {
		for name, file := range binaries {
			if binaryHash(t, file) != hashes[name] {
				t.Errorf("supplied %s changed", name)
			}
		}
	}()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("fixture credential unavailable")
	}
	providerToken := hex.EncodeToString(secret)
	tokenFile := filepath.Join(private, "provider.token")
	if os.WriteFile(tokenFile, []byte(providerToken+"\n"), 0600) != nil {
		t.Fatal("private fixture token unavailable")
	}
	provider := newRunSelectionHTTPSFixture(t, private, providerToken)
	env := []string{"TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + provider.proxy, "HTTPS_PROXY=" + provider.proxy, "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE=" + provider.ca, "SSL_CERT_DIR=" + private}
	gitLog, reviewLog := filepath.Join(private, "git.argv"), filepath.Join(private, "review.argv")
	gitStub, reviewStub := filepath.Join(private, "git-stub"), filepath.Join(private, "review-stub")
	for file, log := range map[string]string{gitStub: gitLog, reviewStub: reviewLog} {
		if os.WriteFile(file, []byte("#!/bin/sh\nprintf 'invocation\\n' >> "+runSelectionShellQuote(log)+"\nexit 2\n"), 0700) != nil {
			t.Fatal("trusted hold fixture unavailable")
		}
	}
	catalogFile := filepath.Join(private, "catalog.json")
	catalog, _ := json.Marshal(map[string]any{"version": 1, "capabilities": []any{map[string]any{"id": "explicit-order", "account": "synthetic-order-account", "organization": "synthetic-org", "channels": []string{"cli"}, "github_credential_file": tokenFile, "git_executable": gitStub, "cli_executable": reviewStub, "authentication_environment": map[string]string{}}}})
	if os.WriteFile(catalogFile, catalog, 0600) != nil {
		t.Fatal("private operator catalog unavailable")
	}
	inputs := map[string][32]byte{}
	for _, file := range []string{tokenFile, catalogFile, gitStub, reviewStub, provider.ca} {
		inputs[file] = binaryHash(t, file)
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, workerFile := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	seedRunSelectionConfiguration(t, ctx, data, catalogFile, project, worker, strings.TrimSpace(string(admin)), 0, []int64{2, 3}, true)
	initial := readUpgradeState(t, ctx, data)
	workerToken, err := os.ReadFile(workerFile)
	if err != nil {
		t.Fatal("private worker credential unavailable")
	}
	safe := func(body []byte) {
		t.Helper()
		if len(body) > 64<<10 || bytes.Contains(body, bytes.TrimSpace(admin)) || bytes.Contains(body, bytes.TrimSpace(workerToken)) || bytes.Contains(body, []byte(providerToken)) {
			t.Fatal("unbounded or private transport output")
		}
	}
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env, "--review-catalog", catalogFile)
	target, _ := url.Parse("http://" + daemon.address)
	var targetMu sync.RWMutex
	forward := httputil.NewSingleHostReverseProxy(target)
	forward.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	forward.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }
	var requests, posts, unexpected atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+project+"/reviews/requests" {
			posts.Add(1)
			body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
			var fields map[string]json.RawMessage
			if err != nil || len(body) > 4096 || json.Unmarshal(body, &fields) != nil || len(fields) != 4 && len(fields) != 5 || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != bootstrap.instance || r.Header.Get("Authorization") != "Bearer "+string(bytes.TrimSpace(workerToken)) {
				unexpected.Add(1)
				w.WriteHeader(403)
				return
			}
			if flag, present := fields["allow_out_of_order"]; present && !bytes.Equal(flag, []byte("true")) {
				unexpected.Add(1)
				w.WriteHeader(403)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		} else if r.Method != http.MethodGet {
			unexpected.Add(1)
			w.WriteHeader(403)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := filepath.Join(private, "profiles.json")
	cliCalls := 0
	call := func(args ...string) json.RawMessage {
		t.Helper()
		body := runCLI(t, ctx, binaries["fc"], env, append([]string{"--profiles-file", profiles, "--profile", "operator"}, args...)...)
		cliCalls++
		safe(body)
		return body
	}
	call("profile", "create", "--server", gateway.URL, "--instance", bootstrap.instance, "--project", project, "--worker", worker, "--credential-file", workerFile)
	profileHash := binaryHash(t, profiles)
	requestArgs := func(key string, allow bool) []string {
		args := []string{"reviews", "request", "3", "--key", key, "--expect-config", "1", "--channel", "cli", "--head", strings.Repeat("a", 40), "--wait-seconds", "0"}
		if allow {
			args = append(args, "--out-of-order")
		}
		return args
	}
	read := func(receipt client.ReviewRequestReceipt, allow bool) client.ReviewOperation {
		t.Helper()
		var op client.ReviewOperation
		decode(t, call("reviews", "operation", receipt.ID, "--wait-seconds", "2"), &op)
		if op.ID != receipt.ID || op.ProjectID != project || op.WorkerID != worker || op.PullNumber != 3 || op.Channel != "cli" || op.HeadSHA != strings.Repeat("a", 40) || op.AllowOutOfOrder != allow || op.State != "held" || op.Reason != "source_unavailable" {
			t.Fatal("operation lost retained ordering intent or deliberate hold")
		}
		return op
	}
	defaultBody := call(requestArgs("default-order", false)...)
	var defaultReceipt client.ReviewRequestReceipt
	decode(t, defaultBody, &defaultReceipt)
	defaultOperation := read(defaultReceipt, false)
	command := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profiles, "--profile", "operator")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "packaged-explicit-order", Version: "1"}, nil)
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	session, err := sdk.Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stop()
	if err != nil {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Fatal("actual MCP initialization failed")
	}
	var closeOnce sync.Once
	joined := make(chan struct{})
	var closeErr error
	closeMCP := func() {
		closeOnce.Do(func() { go func() { closeErr = session.Close(); close(joined) }() })
		select {
		case <-joined:
			if closeErr != nil || command.ProcessState == nil || !command.ProcessState.Success() || stderr.len() != 0 {
				t.Error("owned MCP process did not close cleanly")
			}
		case <-time.After(stepTimeout):
			_ = command.Process.Kill()
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Error("owned MCP process did not join")
			}
			t.Error("owned MCP shutdown exceeded bound")
		}
	}
	t.Cleanup(closeMCP)
	mcpCalls := 0
	tool := func(name string, args any, wantError string) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		result, err := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		mcpCalls++
		if err != nil || result == nil || result.IsError != (wantError != "") || len(result.Content) != 1 {
			t.Fatal("actual packaged MCP call failed")
		}
		body, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal("MCP result unavailable")
		}
		safe(body)
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok || !jsonEqual(body, []byte(text.Text)) {
			t.Fatal("MCP compatibility text differs")
		}
		if wantError != "" {
			var diagnostic struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			decode(t, body, &diagnostic)
			if diagnostic.Error.Code != wantError {
				t.Fatal("MCP refusal classification differs")
			}
		}
		return body
	}
	arguments := func(key string, allow bool) map[string]any {
		return map[string]any{"idempotency_key": key, "expected_configuration_revision": 1, "pull_number": 3, "channel": "cli", "head_sha": strings.Repeat("a", 40), "wait_seconds": 0, "allow_out_of_order": allow}
	}
	var explicitReceipt client.ReviewRequestReceipt
	explicitBody := tool("review_request", arguments("explicit-order", true), "")
	decode(t, explicitBody, &explicitReceipt)
	if explicitReceipt.ID == defaultReceipt.ID || explicitReceipt.State != "queued" || explicitReceipt.Revision != 1 {
		t.Fatal("explicit request did not retain a distinct original receipt")
	}
	explicitOperation := read(explicitReceipt, true)
	providerBefore := provider.inventory()
	gitBefore, err := os.ReadFile(gitLog)
	if err != nil || bytes.Count(gitBefore, []byte("invocation\n")) != 2 || !reflect.DeepEqual(providerBefore, map[string]int{"GET /repositories/991": 2, "GET /repos/synthetic/packaged-smoke/pulls/3": 2}) {
		t.Fatal("request did not stop at the bounded source/Git fixture")
	}
	if !jsonEqual(defaultBody, tool("review_request", arguments("default-order", false), "")) || !jsonEqual(explicitBody, call(requestArgs("explicit-order", true)...)) {
		t.Fatal("cross-client original-key replay changed receipt")
	}
	tool("review_request", arguments("explicit-order", false), "idempotency_conflict")
	// Actual CLI parse refusal must happen before reading even a nonexistent profile.
	beforeHTTP := requests.Load()
	bounded, stop = context.WithTimeout(ctx, stepTimeout)
	invalid := exec.CommandContext(bounded, binaries["fc"], "--profiles-file", filepath.Join(private, "missing.json"), "--profile", "absent", "reviews", "start", "--channel", "cli", "--key", "automatic", "--out-of-order")
	invalid.Env, invalid.WaitDelay = env, time.Second
	invalidOut, invalidErr := &boundedOutput{}, &boundedOutput{}
	invalid.Stdout, invalid.Stderr = invalidOut, invalidErr
	err = invalid.Run()
	stop()
	cliCalls++
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != cli.ExitUsage || invalidOut.len() != 0 || !bytes.Contains(invalidErr.bytes(), []byte("invalid_arguments")) {
		t.Fatal("automatic CLI accepted bypass or read credentials")
	}
	safe(invalidErr.bytes())
	tool("review_start", map[string]any{"idempotency_key": "automatic", "channel": "cli", "allow_out_of_order": true}, "invalid_arguments")
	if requests.Load() != beforeHTTP {
		t.Fatal("automatic bypass refusal contacted service")
	}
	var mcpOperation client.ReviewOperation
	decode(t, tool("review_operation_read", map[string]any{"operation_id": explicitReceipt.ID}, ""), &mcpOperation)
	if mcpOperation != explicitOperation {
		t.Fatal("MCP read changed operation identity")
	}
	closeMCP()
	daemon.stop(t)
	retained := readUpgradeState(t, ctx, data)
	if retained.counts["review_operations"] != 2 || retained.counts["review_execution_attempts"] != 2 || retained.counts["review_execution_reservations"] != 0 || retained.counts["review_selections"] != 0 || retained.counts["review_execution_evidence"] != 0 || retained.counts["review_round_completions"] != 0 || retained.tables["review_execution_config"] != initial.tables["review_execution_config"] || retained.tables["review_round_controls"] != initial.tables["review_round_controls"] {
		t.Fatal("explicit request fabricated credit, selection or configuration changes")
	}
	restarted := startDaemon(t, ctx, binaries["fcd"], data, env, "--review-catalog", catalogFile)
	next, _ := url.Parse("http://" + restarted.address)
	targetMu.Lock()
	target = next
	targetMu.Unlock()
	if restarted.instance != bootstrap.instance || read(defaultReceipt, false) != defaultOperation || read(explicitReceipt, true) != explicitOperation || !bytes.Equal(defaultBody, call(requestArgs("default-order", false)...)) || !jsonEqual(explicitBody, call(requestArgs("explicit-order", true)...)) {
		t.Fatal("restart changed retained intent or original replay")
	}
	restarted.stop(t)
	if !reflect.DeepEqual(retained, readUpgradeState(t, ctx, data)) || !reflect.DeepEqual(providerBefore, provider.inventory()) {
		t.Fatal("replay/read/restart changed native state or reacquired source")
	}
	if body, err := os.ReadFile(gitLog); err != nil || !bytes.Equal(body, gitBefore) {
		t.Fatal("replay/read/restart launched Git again")
	}
	if _, err := os.Stat(reviewLog); !errors.Is(err, os.ErrNotExist) || provider.forbidden.Load() != 0 || unexpected.Load() != 0 || posts.Load() != 7 || binaryHash(t, profiles) != profileHash {
		t.Fatal("unexpected review execution, request or profile mutation")
	}
	for file, hash := range inputs {
		if binaryHash(t, file) != hash {
			t.Fatal("immutable fixture input changed")
		}
	}
	t.Logf("packaged explicit order passed: total=%s cli_calls=%d mcp_calls=%d application_http=%d request_posts=7 loopback_source_gets=4 git_stub_calls=2 review_calls=0 native_operations=2 owned_processes=0", time.Since(started).Round(time.Millisecond), cliCalls, mcpCalls, requests.Load())
}
