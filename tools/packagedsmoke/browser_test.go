//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestPackagedBrowserSmoke consumes the existing embedded-browser daemon. It
// never builds assets/binaries or installs Chromium.
func TestPackagedBrowserSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	// Run owned-daemon cleanups before cancellation, including assertion failures.
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private browser fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("FC_SMOKE_BIN_DIR must be absolute")
	}
	binary := filepath.Join(binDir, "fcd")
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		t.Fatal("packaged fcd missing; build before browser smoke")
	}
	hash := binaryHash(t, binary)
	t.Logf("browser packaged input fcd sha256=%x", hash)
	defer func() {
		if binaryHash(t, binary) != hash {
			t.Error("browser smoke modified fcd")
		}
	}()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for packaged browser smoke")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("Node executable unavailable")
	}
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	// Chromium engine traffic (for example its own Google CONNECT probes) is
	// blocked and counted separately; page routes still reject every foreign
	// origin, and daemon traffic must remain zero.
	var engine atomic.Int64
	browserDeny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { engine.Add(1); w.WriteHeader(502) }))
	defer browserDeny.Close()
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binary, data, env)
	health(t, ctx, bootstrap.address)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("private bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, otherWorker, credentialFile := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, binary, data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("browser fixture identity changed")
	}
	health(t, ctx, daemon.address)
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if executable != "" {
		if !filepath.IsAbs(executable) {
			t.Fatal("FC_BROWSER_EXECUTABLE must be absolute")
		}
		if info, err := os.Stat(executable); err != nil || !info.Mode().IsRegular() {
			t.Fatal("browser executable override unavailable")
		}
		t.Log("browser smoke uses explicit FC_BROWSER_EXECUTABLE override")
	} else {
		t.Log("browser smoke uses Playwright's installed default Chromium")
	}
	// Preserve only the browser installation selector, never installed profiles or
	// application/provider credentials. HOME remains the disposable fixture.
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal("Playwright browser cache unavailable")
		}
		browserPath = filepath.Join(cache, "ms-playwright")
	}
	if !filepath.IsAbs(browserPath) {
		t.Fatal("PLAYWRIGHT_BROWSERS_PATH must be absolute")
	}
	env = append(env, "PLAYWRIGHT_BROWSERS_PATH="+browserPath)
	config := map[string]string{"origin": "http://" + daemon.address, "project": project, "worker": worker, "other_worker": otherWorker, "instance": daemon.instance, "admin_file": adminFile, "credential_file": credentialFile, "executable": executable, "proxy": browserDeny.URL}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal("browser fixture configuration unavailable")
	}
	configFile := filepath.Join(private, "browser.json")
	if os.WriteFile(configFile, raw, 0600) != nil {
		t.Fatal("private browser configuration unavailable")
	}
	script, _ := filepath.Abs("browser.mjs")
	command := exec.Command(node, script, configFile)
	command.Env, command.SysProcAttr, command.WaitDelay = env, &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if command.Start() != nil {
		t.Fatal("packaged browser driver could not start")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	joined := false
	// Chromium's public launchServer API exposes its own process-group leader;
	// kill only these two exclusively owned groups, including on driver failure.
	defer func() {
		if pid := output.browserPID(); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if !joined {
			select {
			case <-done:
			case <-time.After(stepTimeout):
				t.Error("owned browser driver did not join")
			}
		}
	}()
	select {
	case err = <-done:
		joined = true
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
			joined = true
		case <-time.After(stepTimeout):
		}
		t.Fatal("packaged browser workload exceeded 45 seconds")
	}
	if err != nil || stderr.len() != 0 || output.len() > 64<<10 {
		t.Fatalf("packaged browser failed at phase=%s; raw output withheld to protect fixture credentials", output.safePhase())
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State                    string `json:"state"`
		FixtureWrites            int    `json:"fixture_writes"`
		SessionPosts             int    `json:"session_posts"`
		SessionDeletes           int    `json:"session_deletes"`
		ActionWrites             int    `json:"action_writes"`
		MetadataReconnect        bool   `json:"metadata_reconnect"`
		InjectedMetadataFailures int    `json:"injected_metadata_failures"`
		InjectedConsoleErrors    int    `json:"injected_console_errors"`
		PresentationReads        int    `json:"presentation_reads"`
		MetadataStreams          int    `json:"metadata_streams"`
		InboxStreams             int    `json:"inbox_streams"`
	}
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || result.FixtureWrites != 17 || result.SessionPosts != 2 || result.SessionDeletes != 2 || result.ActionWrites != 7 || !result.MetadataReconnect || result.InjectedMetadataFailures != 1 || result.InjectedConsoleErrors < 0 || result.InjectedConsoleErrors > 1 || result.PresentationReads != 4 || result.MetadataStreams != 3 || result.InboxStreams != 2 {
		t.Fatal("invalid packaged browser result")
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("browser smoke attempted external HTTP")
	}
	t.Logf("packaged browser passed: total=%s viewport=390 fixture_writes=%d session_posts=2 session_deletes=2 browser_action_writes=7 metadata_reconnect=true injected_metadata_failures=1 injected_console_errors=%d presentation_reads=4 metadata_streams=3 inbox_streams=2 daemon_external_http=0 page_external_http=0 blocked_engine_requests=%d owned_processes=0", time.Since(started).Round(time.Millisecond), result.FixtureWrites, result.InjectedConsoleErrors, engine.Load())
}

// The only early driver output is its owned Chromium group ID. Keep all output
// bounded and never include it in diagnostics.
type browserOutput struct {
	boundedOutput
	pidMu sync.Mutex
	pid   int
}

func (o *browserOutput) Write(p []byte) (int, error) {
	n, err := o.boundedOutput.Write(p)
	body := o.bytes()
	if end := bytes.IndexByte(body, '\n'); end >= 0 {
		var record struct {
			Event string `json:"event"`
			PID   int    `json:"pid"`
		}
		if json.Unmarshal(body[:end], &record) == nil && record.Event == "browser_started" && record.PID > 1 {
			o.pidMu.Lock()
			o.pid = record.PID
			o.pidMu.Unlock()
		}
	}
	return n, err
}
func (o *browserOutput) browserPID() int { o.pidMu.Lock(); defer o.pidMu.Unlock(); return o.pid }

func (o *browserOutput) safePhase() string {
	for _, line := range bytes.Split(o.bytes(), []byte("\n")) {
		var record struct {
			State                    string `json:"state"`
			Phase                    string `json:"phase"`
			Checkpoint               string `json:"checkpoint"`
			FailureKind              string `json:"failure_kind"`
			SessionPosts             int    `json:"session_posts"`
			SessionDeletes           int    `json:"session_deletes"`
			ActionWrites             int    `json:"action_writes"`
			PresentationReads        int    `json:"presentation_reads"`
			MetadataStreams          int    `json:"metadata_streams"`
			InboxStreams             int    `json:"inbox_streams"`
			InjectedMetadataFailures int    `json:"injected_metadata_failures"`
			InjectedConsoleErrors    int    `json:"injected_console_errors"`
			Errors                   int    `json:"errors"`
			External                 int    `json:"external"`
		}
		if json.Unmarshal(line, &record) != nil || record.State != "FAIL" {
			continue
		}
		switch record.Phase {
		case "configuration", "launch", "connect", "fixture-update", "live-update", "metadata-failure", "metadata-reconnect", "metadata-live-update", "live-jobs", "live-workers", "live-notes", "live-reviews", "reload", "disconnect", "explicit-actions-connect", "explicit-actions-send", "explicit-actions-settings", "explicit-actions-uncertain":
			if record.Phase == "live-reviews" {
				switch record.Checkpoint {
				case "round-grant", "round-allowance", "round-close", "round-closed", "round-observation-update":
					return record.Phase + "/" + record.Checkpoint
				}
			}
			if record.Phase == "metadata-failure" {
				switch record.Checkpoint {
				case "metadata-notice", "metadata-initial-counts", "metadata-inbox-ready":
					valid := true
					for _, n := range []int{record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.InjectedMetadataFailures, record.InjectedConsoleErrors, record.Errors, record.External} {
						valid = valid && n >= 0 && n <= 1000
					}
					if valid {
						return fmt.Sprintf("metadata-failure/%s presentation_reads=%d metadata_streams=%d inbox_streams=%d injected_metadata_failures=%d injected_console_errors=%d errors=%d external=%d", record.Checkpoint, record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.InjectedMetadataFailures, record.InjectedConsoleErrors, record.Errors, record.External)
					}
				}
			}
			if strings.HasPrefix(record.Phase, "explicit-actions-") {
				validCheckpoint := false
				switch record.Phase + "/" + record.Checkpoint {
				case "explicit-actions-connect/connect-history", "explicit-actions-connect/connect-project", "explicit-actions-connect/connect-credential", "explicit-actions-connect/connect-consent", "explicit-actions-connect/connect-submit", "explicit-actions-connect/connect-heading", "explicit-actions-connect/connect-worker", "explicit-actions-connect/connect-inbox", "explicit-actions-connect/incoming-write", "explicit-actions-connect/incoming-row", "explicit-actions-connect/incoming-unread", "explicit-actions-connect/incoming-seen-submit", "explicit-actions-connect/incoming-seen", "explicit-actions-connect/incoming-ack-submit", "explicit-actions-connect/incoming-ack",
					"explicit-actions-send/send-compose", "explicit-actions-send/reply-compose", "explicit-actions-settings/settings-save", "explicit-actions-settings/settings-reload", "explicit-actions-uncertain/uncertain-compose", "explicit-actions-uncertain/uncertain-commit", "explicit-actions-uncertain/uncertain-return", "explicit-actions-uncertain/uncertain-discard", "explicit-actions-uncertain/uncertain-retry", "explicit-actions-uncertain/uncertain-disconnect":
					validCheckpoint = true
				}
				validKind := record.FailureKind == "timeout" || record.FailureKind == "assertion" || record.FailureKind == "driver"
				validCounts := true
				for _, n := range []int{record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.InjectedMetadataFailures, record.InjectedConsoleErrors, record.Errors, record.External, record.SessionPosts, record.SessionDeletes, record.ActionWrites} {
					validCounts = validCounts && n >= 0 && n <= 1000
				}
				if validCheckpoint && validKind && validCounts {
					return fmt.Sprintf("%s/%s kind=%s session_posts=%d session_deletes=%d action_writes=%d errors=%d external=%d", record.Phase, record.Checkpoint, record.FailureKind, record.SessionPosts, record.SessionDeletes, record.ActionWrites, record.Errors, record.External)
				}
			}
			return record.Phase
		}
	}
	return "driver-start-or-cleanup"
}

func TestBrowserOutputSafeActionFailureDiagnostics(t *testing.T) {
	const secret = "PRIVATE_TOKEN_https://private.invalid/secret"
	base := map[string]any{"state": "FAIL", "phase": "explicit-actions-connect", "checkpoint": "connect-submit", "failure_kind": "timeout", "session_posts": 2, "session_deletes": 1, "action_writes": 0, "errors": 0, "external": 0}
	for _, test := range []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"safe timeout", func(map[string]any) {}, "explicit-actions-connect/connect-submit kind=timeout session_posts=2 session_deletes=1 action_writes=0 errors=0 external=0"},
		{"safe assertion", func(v map[string]any) { v["failure_kind"] = "assertion" }, "explicit-actions-connect/connect-submit kind=assertion session_posts=2 session_deletes=1 action_writes=0 errors=0 external=0"},
		{"safe driver", func(v map[string]any) {
			v["failure_kind"] = "driver"
			v["message"] = secret
			v["stack"] = secret
			v["locator"] = secret
		}, "explicit-actions-connect/connect-submit kind=driver session_posts=2 session_deletes=1 action_writes=0 errors=0 external=0"},
		{"private checkpoint", func(v map[string]any) { v["checkpoint"] = secret }, "explicit-actions-connect"},
		{"private kind", func(v map[string]any) { v["failure_kind"] = secret }, "explicit-actions-connect"},
		{"cross phase checkpoint", func(v map[string]any) { v["checkpoint"] = "uncertain-retry" }, "explicit-actions-connect"},
		{"private phase", func(v map[string]any) { v["phase"] = secret }, "driver-start-or-cleanup"},
		{"negative count", func(v map[string]any) { v["session_posts"] = -1 }, "explicit-actions-connect"},
		{"oversized count", func(v map[string]any) { v["action_writes"] = 1001 }, "explicit-actions-connect"},
		{"unprinted oversized count", func(v map[string]any) { v["metadata_streams"] = 1001 }, "explicit-actions-connect"},
		{"private count", func(v map[string]any) { v["errors"] = secret }, "driver-start-or-cleanup"},
		{"fractional count", func(v map[string]any) { v["external"] = 0.5 }, "driver-start-or-cleanup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := map[string]any{}
			for k, v := range base {
				value[k] = v
			}
			test.edit(value)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			out := &browserOutput{}
			_, _ = out.Write(append(raw, '\n'))
			got := out.safePhase()
			if got != test.want || strings.Contains(got, secret) {
				t.Fatal("unsafe or incomplete failure diagnostics", got)
			}
		})
	}
	for _, phase := range []string{"explicit-actions-send", "explicit-actions-settings", "explicit-actions-uncertain"} {
		checkpoint := map[string]string{"explicit-actions-send": "reply-compose", "explicit-actions-settings": "settings-reload", "explicit-actions-uncertain": "uncertain-retry"}[phase]
		raw, _ := json.Marshal(map[string]any{"state": "FAIL", "phase": phase, "checkpoint": checkpoint, "failure_kind": "assertion"})
		out := &browserOutput{}
		_, _ = out.Write(raw)
		if got := out.safePhase(); !strings.HasPrefix(got, phase+"/"+checkpoint+" kind=assertion ") {
			t.Fatal("known action stage lost", got)
		}
	}
}

// Exercise the same helper as the driver with genuine Playwright failures.
// No daemon, application credentials or provider inputs are needed.
func TestBrowserFailureDiagnosticsRetained(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	private := privateFixtureDirectory(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for browser diagnostic proof")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("Node executable unavailable")
	}
	helper, _ := filepath.Abs("browser-diagnostics.mjs")
	playwright, _ := filepath.Abs("../../webapp/node_modules/@playwright/test/index.mjs")
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("browser cache unavailable")
	}
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		browserPath = filepath.Join(cache, "ms-playwright")
	}
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if !filepath.IsAbs(browserPath) || executable != "" && !filepath.IsAbs(executable) {
		t.Fatal("browser installation selectors must be absolute")
	}
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	config, err := json.Marshal(map[string]string{"helper": helper, "playwright": playwright, "directory": filepath.Join(private, "diagnostics"), "proxy": deny.URL, "executable": executable})
	if err != nil {
		t.Fatal("browser diagnostic configuration unavailable")
	}
	configFile := filepath.Join(private, "diagnostics.json")
	if os.WriteFile(configFile, config, 0600) != nil {
		t.Fatal("browser diagnostic configuration unavailable")
	}
	const probe = `
import fs from 'node:fs/promises';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
import assert from 'node:assert/strict';
let server, browser, passed=false;
try {
  const config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
  const {classifyBrowserFailure}=await import(pathToFileURL(config.helper));
  const {chromium,expect}=await import(pathToFileURL(config.playwright));
  server=await chromium.launchServer({headless:true,timeout:5000,proxy:{server:config.proxy},...(config.executable?{executablePath:config.executable}:{}),args:['--disable-background-networking','--no-first-run']});
  console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
  browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});
  const context=await browser.newContext();let requests=0;
  await context.route('**/*',route=>{requests++;return route.abort();});
  const page=await context.newPage();
  const secret='PRIVATE_SENTINEL';
  await page.setContent('<input value="'+secret+'"/>');
  await page.evaluate(secret=>{location.hash=secret;},secret);
  await context.addCookies([{name:'private',value:secret,url:'https://fixture.invalid'}]);
  let matcher;
  try {await expect(page.locator('input')).toHaveValue('',{timeout:50});} catch(error) {matcher=error;}
  assert.equal(matcher?.matcherResult?.name,'toHaveValue');
  assert.equal(classifyBrowserFailure(matcher),'assertion');
  assert.equal(classifyBrowserFailure(new assert.AssertionError({message:secret})),'assertion');
  assert.equal(classifyBrowserFailure(new Error('assertion')),'assertion');
  assert.equal(classifyBrowserFailure(new Error(secret)),'driver');
  let timeout;
  try {await page.locator('#absent').waitFor({timeout:50});} catch(error) {timeout=error;}
  assert.equal(timeout?.name,'TimeoutError');
  assert.equal(classifyBrowserFailure(timeout),'timeout');
  await context.close();passed=true;
} catch {process.exitCode=1;}
finally {
  try {if(browser) await browser.close();if(server) await server.close();}
  catch {passed=false;process.exitCode=1;}
}
console.log(JSON.stringify({state:passed?'PASS':'FAIL',matcher_assertion:passed}));
`
	command := exec.CommandContext(ctx, node, "--input-type=module", "-", configFile)
	command.Stdin = strings.NewReader(probe)
	command.Env = []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "PATH=", "PLAYWRIGHT_BROWSERS_PATH=" + browserPath, "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	command.SysProcAttr, command.WaitDelay = &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if command.Start() != nil {
		t.Fatal("browser diagnostic probe could not start")
	}
	defer func() {
		if pid := output.browserPID(); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}()
	err = command.Wait()
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State            string `json:"state"`
		MatcherAssertion bool   `json:"matcher_assertion"`
	}
	if err != nil || stderr.len() != 0 || external.Load() != 0 || output.browserPID() < 2 || len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || !result.MatcherAssertion {
		t.Fatal("browser diagnostic failure-path proof failed; raw output withheld")
	}
}

func TestBrowserOutputSafeReviewProgressCheckpoint(t *testing.T) {
	for _, checkpoint := range []string{"round-grant", "round-allowance", "round-close", "round-closed", "round-observation-update"} {
		var out browserOutput
		body, _ := json.Marshal(map[string]string{"state": "FAIL", "phase": "live-reviews", "checkpoint": checkpoint, "message": "PRIVATE_TOKEN"})
		_, _ = out.Write(append(body, '\n'))
		if got := out.safePhase(); got != "live-reviews/"+checkpoint {
			t.Fatalf("unexpected safe checkpoint %q", got)
		}
	}
	var out browserOutput
	_, _ = out.Write([]byte(`{"state":"FAIL","phase":"live-reviews","checkpoint":"PRIVATE_TOKEN"}` + "\n"))
	if got := out.safePhase(); got != "live-reviews" {
		t.Fatalf("unsafe checkpoint %q", got)
	}
}
