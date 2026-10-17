//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
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
	project, worker, _, credentialFile := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
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
	config := map[string]string{"origin": "http://" + daemon.address, "project": project, "worker": worker, "instance": daemon.instance, "admin_file": adminFile, "credential_file": credentialFile, "executable": executable, "proxy": browserDeny.URL}
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
		State          string `json:"state"`
		FixtureWrites  int    `json:"fixture_writes"`
		SessionPosts   int    `json:"session_posts"`
		SessionDeletes int    `json:"session_deletes"`
	}
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || result.FixtureWrites != 1 || result.SessionPosts != 1 || result.SessionDeletes != 1 {
		t.Fatal("invalid packaged browser result")
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("browser smoke attempted external HTTP")
	}
	t.Logf("packaged browser passed: total=%s viewport=390 fixture_writes=1 session_posts=1 session_deletes=1 daemon_external_http=0 page_external_http=0 blocked_engine_requests=%d owned_processes=0", time.Since(started).Round(time.Millisecond), engine.Load())
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
			State string `json:"state"`
			Phase string `json:"phase"`
		}
		if json.Unmarshal(line, &record) != nil || record.State != "FAIL" {
			continue
		}
		switch record.Phase {
		case "configuration", "launch", "connect", "fixture-update", "live-update", "reload", "disconnect":
			return record.Phase
		}
	}
	return "driver-start-or-cleanup"
}
