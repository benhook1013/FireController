//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestPackagedPublicBrowserSmoke consumes the already-built daemon and embedded
// assets. Publication and private fixtures use authenticated APIs; page clients
// remain anonymous and never initialize the private application or provider work.
func TestPackagedPublicBrowserSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private public-browser fixture unavailable")
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
		t.Fatal("packaged fcd missing; build before public browser smoke")
	}
	hash := binaryHash(t, binary)
	t.Logf("public browser packaged input fcd sha256=%x", hash)
	defer func() {
		if binaryHash(t, binary) != hash {
			t.Error("public browser smoke modified fcd")
		}
	}()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for public browser smoke")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("Node executable unavailable")
	}
	var external, engine atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
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
	project, worker, otherWorker, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, binary, data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("public browser fixture identity changed")
	}
	health(t, ctx, daemon.address)
	// A real proxy compresses finite public JSON. Browser Fetch must expose its
	// decoded bytes while preserving Content-Encoding; SSE remains unbuffered.
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	var compressed atomic.Int64
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Request.URL.Path, "/api/v1/public/projects/") || strings.HasSuffix(response.Request.URL.Path, "/events") {
			return nil
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, (512<<10)+1))
		_ = response.Body.Close()
		if err != nil || len(raw) > 512<<10 {
			return io.ErrUnexpectedEOF
		}
		var body bytes.Buffer
		writer := gzip.NewWriter(&body)
		if _, err = writer.Write(raw); err != nil {
			return err
		}
		if err = writer.Close(); err != nil {
			return err
		}
		response.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
		response.ContentLength = int64(body.Len())
		response.Header.Set("Content-Encoding", "gzip")
		response.Header.Set("Content-Length", strconv.Itoa(body.Len()))
		compressed.Add(1)
		return nil
	}
	gateway := httptest.NewServer(proxy)
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if executable != "" {
		if info, err := os.Stat(executable); !filepath.IsAbs(executable) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("browser executable override unavailable")
		}
	}
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
	config := map[string]string{"origin": gateway.URL, "project": project, "worker": worker, "other_worker": otherWorker, "instance": daemon.instance, "admin_file": adminFile, "executable": executable, "proxy": browserDeny.URL}
	if directory := os.Getenv("FC_PUBLIC_BROWSER_PROOF_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			t.Fatal("FC_PUBLIC_BROWSER_PROOF_DIR must be absolute")
		}
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal("public browser proof directory unavailable")
		}
		config["proof_directory"] = directory
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal("public browser configuration unavailable")
	}
	configFile := filepath.Join(private, "public-browser.json")
	if err := os.WriteFile(configFile, raw, 0600); err != nil {
		t.Fatal("private public browser configuration unavailable")
	}
	script, _ := filepath.Abs("public_browser.mjs")
	command := exec.Command(node, script, configFile)
	command.Env, command.SysProcAttr, command.WaitDelay = env, &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if err := command.Start(); err != nil {
		t.Fatal("public browser driver could not start")
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
				t.Error("owned public browser driver did not join")
			}
		}
	}()
	select {
	case err = <-done:
		joined = true
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		t.Fatal("public browser workload exceeded45 seconds")
	}
	if err != nil || stderr.len() != 0 || output.len() > 64<<10 {
		phase := "driver-start-or-cleanup"
		for _, line := range bytes.Split(output.bytes(), []byte("\n")) {
			var record struct{ State, Phase string }
			if json.Unmarshal(line, &record) == nil && record.State == "FAIL" {
				for _, allowed := range []string{"configuration", "fixture", "launch", "desktop", "private-quiet", "live-summary", "frozen-history", "phone", "unpublish"} {
					if record.Phase == allowed {
						phase = allowed
					}
				}
			}
		}
		t.Fatalf("public browser failed at phase=%s; raw fixture output withheld", phase)
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State            string `json:"state"`
		FixtureCalls     int    `json:"fixture_calls"`
		PublicRequests   int    `json:"public_requests"`
		PrivateRequests  int    `json:"private_requests"`
		PageWrites       int    `json:"page_writes"`
		PageErrors       int    `json:"page_errors"`
		ExternalRequests int    `json:"external_requests"`
		Desktop          int    `json:"desktop"`
		Phone            int    `json:"phone"`
	}
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || result.FixtureCalls != 10 || result.PrivateRequests != 0 || result.PageWrites != 0 || result.PageErrors != 0 || result.ExternalRequests != 0 || result.Desktop != 1280 || result.Phone != 390 {
		t.Fatal("invalid public browser result")
	}
	daemon.stop(t)
	if external.Load() != 0 || compressed.Load() < 6 {
		t.Fatal("public browser attempted external HTTP or did not exercise compressed public JSON")
	}
	t.Logf("public browser passed total=%s desktop=1280 phone=390 fixture_calls=%d public_requests=%d compressed_json=%d private_requests=0 page_writes=0 external_http=0 blocked_engine_requests=%d owned_processes=0", time.Since(started).Round(time.Millisecond), result.FixtureCalls, result.PublicRequests, compressed.Load(), engine.Load())
}
