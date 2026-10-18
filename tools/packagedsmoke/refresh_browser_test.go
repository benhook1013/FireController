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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

// TestPackagedRefreshBrowserSmoke consumes immutable packaged assets; it never builds.
func TestPackagedRefreshBrowserSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private refresh fixture unavailable")
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
	t.Logf("refresh packaged input fcd sha256=%x", hash)
	defer func() {
		if binaryHash(t, binary) != hash {
			t.Error("refresh proof modified fcd")
		}
	}()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("Node unavailable")
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
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	adminBytes, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(adminBytes)) == 0 {
		t.Fatal("private bootstrap unavailable")
	}
	bootstrap.stop(t)
	admin := strings.TrimSpace(string(adminBytes))
	project, worker, _, _ := seedAccess(t, ctx, data, bootstrap.instance, admin, private)
	firstSecrets, secondSecrets := filepath.Join(private, "project-a"), filepath.Join(private, "project-b")
	for _, directory := range []string{firstSecrets, secondSecrets} {
		if os.Mkdir(directory, 0700) != nil {
			t.Fatal("private credential directory unavailable")
		}
	}
	credential := seedBrowserReadOnlyCredential(t, ctx, data, bootstrap.instance, project, admin, firstSecrets)
	targetProject, targetWorker := seedRefreshProofTarget(t, ctx, data, bootstrap.instance, admin)
	targetCredential := seedBrowserReadOnlyCredential(t, ctx, data, bootstrap.instance, targetProject, admin, secondSecrets)
	if credential == targetCredential {
		t.Fatal("project credential files must be distinct")
	}
	daemon := startDaemon(t, ctx, binary, data, env)
	health(t, ctx, daemon.address)
	if daemon.instance != bootstrap.instance {
		t.Fatal("refresh fixture identity changed")
	}
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if executable != "" {
		info, err := os.Stat(executable)
		if !filepath.IsAbs(executable) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("browser executable override unavailable")
		}
	}
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		browserPath, err = os.UserCacheDir()
		if err != nil {
			t.Fatal("browser cache unavailable")
		}
		browserPath = filepath.Join(browserPath, "ms-playwright")
	}
	if !filepath.IsAbs(browserPath) {
		t.Fatal("browser cache must be absolute")
	}
	env = append(env, "PLAYWRIGHT_BROWSERS_PATH="+browserPath)
	defaults, _ := filepath.Abs("../../internal/presentationmodel/defaults.json")
	config := map[string]string{"origin": "http://" + daemon.address, "project": project, "worker": worker, "target_project": targetProject, "target_worker": targetWorker, "instance": daemon.instance, "admin_file": adminFile, "credential_file": credential, "target_credential_file": targetCredential, "defaults_file": defaults, "executable": executable, "proxy": browserDeny.URL}
	if directory := os.Getenv("FC_BROWSER_PROOF_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			t.Fatal("proof directory must be absolute")
		}
		config["diagnostic_directory"] = directory
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal("private configuration unavailable")
	}
	configFile := filepath.Join(private, "refresh-browser.json")
	if os.WriteFile(configFile, raw, 0600) != nil {
		t.Fatal("private configuration unavailable")
	}
	script, _ := filepath.Abs("refresh_browser.mjs")
	command := exec.Command(node, script, configFile)
	command.Env, command.SysProcAttr, command.WaitDelay = env, &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if command.Start() != nil {
		t.Fatal("refresh browser could not start")
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
				t.Error("owned refresh driver did not join")
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
		t.Fatal("refresh workload exceeded45seconds")
	}
	if err != nil || stderr.len() != 0 || output.len() > 64<<10 {
		for _, line := range bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n")) {
			var failure struct {
				State, Phase string
				Line         int
			}
			if json.Unmarshal(line, &failure) == nil && failure.State == "FAIL" {
				switch failure.Phase {
				case "configuration", "fixture", "connect-a", "closed-feed", "missed-events", "held-refresh", "refresh-result", "late-refresh", "connect-b", "late-result", "logout":
					t.Logf("safe refresh failure phase=%s driver_line=%d", failure.Phase, failure.Line)
				}
			}
		}
		t.Fatal("packaged refresh failed; raw output withheld to protect fixture credentials")
	}
	var result struct {
		State            string `json:"state"`
		FixtureCalls     int    `json:"fixture_calls"`
		SessionPosts     int    `json:"session_posts"`
		SessionDeletes   int    `json:"session_deletes"`
		DomainWrites     int    `json:"domain_writes"`
		Errors           int    `json:"errors"`
		External         int    `json:"external"`
		InjectedFailures int    `json:"injected_failures"`
		InjectedConsole  int    `json:"injected_console"`
		MetadataA        int    `json:"metadata_a"`
		PresentationA    int    `json:"presentation_a"`
		LateResult       string `json:"late_result"`
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || result.FixtureCalls != 6 || result.SessionPosts != 2 || result.SessionDeletes != 2 || result.DomainWrites != 0 || result.Errors != 0 || result.External != 0 || result.InjectedFailures != 1 || result.InjectedConsole > 1 || result.MetadataA != 1 || result.PresentationA != 3 || (result.LateResult != "delivered" && result.LateResult != "cancelled") {
		t.Fatal("invalid refresh browser result")
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("refresh proof attempted external HTTP")
	}
	t.Logf("packaged refresh passed: elapsed=%s fixture_calls=%d session_posts=%d session_deletes=%d domain_writes=0 errors=0 external=0 injected_failures=%d injected_console=%d metadata_a=%d presentation_a=%d late_result=%s", time.Since(started).Round(time.Millisecond), result.FixtureCalls, result.SessionPosts, result.SessionDeletes, result.InjectedFailures, result.InjectedConsole, result.MetadataA, result.PresentationA, result.LateResult)
}

// No installed/provider repository lookup is used for this distinct project.
func seedRefreshProofTarget(t *testing.T, ctx context.Context, data, instance, admin string) (string, string) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("target fixture unavailable")
	}
	defer store.Close()
	if store.InstanceID() != instance {
		t.Fatal("target fixture identity changed")
	}
	lookups := 0
	mux := server.New(store, server.WithRepositoryResolver(func(_ context.Context, fullName string) (server.RepositoryIdentity, error) {
		if fullName != "synthetic/refresh-target-b" {
			t.Fatal("unowned repository identity")
		}
		lookups++
		return server.RepositoryIdentity{ID: 992, FullName: fullName}, nil
	}))
	call := func(path string, body any) string {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal("target JSON unavailable")
		}
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-FC-Instance", instance)
		r.Header.Set("Idempotency-Key", core.NewID("refresh_target"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var receipt struct {
			ID string `json:"id"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" {
			t.Fatal("target setup refused")
		}
		return receipt.ID
	}
	project := call("/api/v1/projects", map[string]any{"name": "Refresh target B", "repository": "synthetic/refresh-target-b"})
	worker := call("/api/v1/projects/"+project+"/workers", map[string]any{"name": "Target worker"})
	if lookups != 1 {
		t.Fatal("unexpected target repository lookups")
	}
	t.Log("target setup: two authenticated handler calls, one synthetic repository lookup; credentials separately scoped")
	return project, worker
}
