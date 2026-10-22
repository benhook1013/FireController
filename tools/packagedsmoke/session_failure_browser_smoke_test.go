//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestPackagedSessionFailureBrowserSmoke checks session renewal failures, live
// credential revocation and cookie eviction against the unchanged supplied daemon.
func TestPackagedSessionFailureBrowserSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private second session browser fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("FC_SMOKE_BIN_DIR must be absolute")
	}
	binary := filepath.Join(binDir, "fcd")
	hash := binaryHash(t, binary)
	t.Logf("second session browser packaged input fcd sha256=%x", hash)
	defer func() {
		if binaryHash(t, binary) != hash {
			t.Error("second session browser smoke modified fcd")
		}
	}()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for second session browser smoke")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("Node executable unavailable")
	}
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	browserDeny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) }))
	defer browserDeny.Close()
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binary, data, env)
	health(t, ctx, bootstrap.address)
	adminBytes, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil || len(bytes.TrimSpace(adminBytes)) == 0 {
		t.Fatal("private second session browser admin credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, otherWorker, writerCredential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(adminBytes)), private)
	credential := seedBrowserReadOnlyCredential(t, ctx, data, bootstrap.instance, project, strings.TrimSpace(string(adminBytes)), private)
	writerBytes, err := os.ReadFile(writerCredential)
	if err != nil {
		t.Fatal("writer fixture unavailable")
	}
	digest := sha256.Sum256(bytes.TrimSpace(writerBytes))
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped credential lookup unavailable")
	}
	var writerID string
	lookupErr := store.DB().QueryRowContext(ctx, "SELECT id FROM credentials WHERE token_sha256=?", hex.EncodeToString(digest[:])).Scan(&writerID)
	store.Close()
	if lookupErr != nil || writerID == "" {
		t.Fatal("writer credential identity unavailable")
	}

	daemon := startDaemon(t, ctx, binary, data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("second session browser fixture identity changed")
	}
	health(t, ctx, daemon.address)
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if executable != "" {
		if !filepath.IsAbs(executable) {
			t.Fatal("FC_BROWSER_EXECUTABLE must be absolute")
		}
		if info, err := os.Stat(executable); err != nil || !info.Mode().IsRegular() {
			t.Fatal("FC_BROWSER_EXECUTABLE unavailable")
		}
	}
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		browserPath, err = os.UserCacheDir()
		if err != nil {
			t.Fatal("Playwright browser cache unavailable")
		}
		browserPath = filepath.Join(browserPath, "ms-playwright")
	}
	if !filepath.IsAbs(browserPath) {
		t.Fatal("PLAYWRIGHT_BROWSERS_PATH must be absolute")
	}
	env = append(env, "PLAYWRIGHT_BROWSERS_PATH="+browserPath)

	config := map[string]string{"origin": "http://" + daemon.address, "project": project, "worker": worker, "other_worker": otherWorker, "instance": daemon.instance, "admin_file": filepath.Join(data, "bootstrap-admin.token"), "credential_file": credential, "executable": executable, "proxy": browserDeny.URL, "writer_credential_file": writerCredential, "writer_credential_id": writerID}
	if directory := os.Getenv("FC_BROWSER_PROOF_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			t.Fatal("FC_BROWSER_PROOF_DIR must be absolute")
		}
		config["diagnostic_directory"] = directory
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal("private second session browser configuration unavailable")
	}
	configFile := filepath.Join(private, "second-session-browser.json")
	if os.WriteFile(configFile, raw, 0600) != nil {
		t.Fatal("private second session browser configuration unavailable")
	}
	script, _ := filepath.Abs("session_failure_browser_smoke.mjs")
	command := exec.Command(node, script, configFile)
	command.Env, command.SysProcAttr, command.WaitDelay = env, &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if command.Start() != nil {
		t.Fatal("second session browser driver could not start")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	joined := false
	defer func() {
		if pid := output.browserPID(); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if !joined {
			select {
			case <-done:
			case <-time.After(stepTimeout):
				t.Error("owned second session browser driver did not join")
			}
		}
	}()
	select {
	case err = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal("second session browser workload deadline")
	}
	t.Logf("second session browser driver: %s", output.bytes())
	if err != nil || stderr.len() != 0 {
		t.Fatalf("second session browser failed; stderr_bytes=%d", stderr.len())
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State  string   `json:"state"`
		Checks []string `json:"checks"`
		Facts  struct {
			Requests           int `json:"requests"`
			FixtureWrites      int `json:"fixture_writes"`
			PageErrors         int `json:"page_errors"`
			External           int `json:"external_requests"`
			DomainWrites       int `json:"browser_domain_writes"`
			Renewals           int `json:"renewals"`
			FailedRenewals     int `json:"failed_renewals"`
			Logouts            int `json:"logouts"`
			APICreatedSessions int `json:"api_created_sessions"`
			EvictedSessions    int `json:"evicted_sessions"`
			CookiePeak         int `json:"eviction_cookie_peak"`
		} `json:"facts"`
	}
	if json.Unmarshal(lines[len(lines)-1], &result) != nil || result.State != "PASS" || len(result.Checks) != 6 || result.Facts.Requests == 0 || result.Facts.FixtureWrites != 4 || result.Facts.PageErrors != 0 || result.Facts.External != 0 || result.Facts.DomainWrites != 0 || result.Facts.Renewals != 2 || result.Facts.FailedRenewals != 1 || result.Facts.Logouts != 5 || result.Facts.APICreatedSessions != 9 || result.Facts.EvictedSessions < 1 || result.Facts.EvictedSessions >= 9 || result.Facts.CookiePeak < 1 || result.Facts.CookiePeak > 8 {
		t.Fatal("second session browser proof receipt invalid")
	}

	if external.Load() != 0 {
		t.Fatal("external daemon request")
	}
	daemon.stop(t)
	for _, pid := range []int{-output.browserPID(), -command.Process.Pid, daemon.command.Process.Pid} {
		if pid == 0 || pid == -1 || syscall.Kill(pid, 0) != syscall.ESRCH {
			t.Error("owned second session browser process survived clean shutdown")
		}
	}
}
