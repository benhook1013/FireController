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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestPackagedEditorialBrowserSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private editorial browser fixture unavailable")
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
	t.Logf("editorial browser packaged input fcd sha256=%x", hash)
	defer func() {
		if binaryHash(t, binary) != hash {
			t.Error("editorial browser smoke modified fcd")
		}
	}()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for editorial browser smoke")
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
		t.Fatal("private editorial browser admin credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, otherWorker, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(adminBytes)), private)
	credential := seedBrowserReadOnlyCredential(t, ctx, data, bootstrap.instance, project, strings.TrimSpace(string(adminBytes)), private)
	daemon := startDaemon(t, ctx, binary, data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("editorial browser fixture identity changed")
	}
	health(t, ctx, daemon.address)
	// Observe the actual daemon requests as well as browser events: owning a
	// response must not add a second GET or forward the anonymous cookie sentinel.
	// The existing sign-out probe contributes one unpublished-location GET before
	// the anonymous context's location/snapshot pair.
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	var publicWireReads, publicWirePrivate atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/public/") {
			publicWireReads.Add(1)
			for _, header := range []string{"Authorization", "Cookie", "X-FC-Browser-Session", "X-FC-Worker", "X-FC-Instance"} {
				if r.Header.Get(header) != "" {
					publicWirePrivate.Add(1)
				}
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
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
	config := map[string]string{"origin": gateway.URL, "project": project, "worker": worker, "other_worker": otherWorker, "instance": daemon.instance, "admin_file": filepath.Join(data, "bootstrap-admin.token"), "credential_file": credential, "executable": executable, "proxy": browserDeny.URL}
	if directory := os.Getenv("FC_BROWSER_PROOF_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			t.Fatal("FC_BROWSER_PROOF_DIR must be absolute")
		}
		config["diagnostic_directory"] = directory
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal("private editorial browser configuration unavailable")
	}
	configFile := filepath.Join(private, "editorial-browser.json")
	if os.WriteFile(configFile, raw, 0600) != nil {
		t.Fatal("private editorial browser configuration unavailable")
	}
	script, _ := filepath.Abs("editorial_browser.mjs")
	command := exec.Command(node, script, configFile)
	command.Env, command.SysProcAttr, command.WaitDelay = env, &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if command.Start() != nil {
		t.Fatal("editorial browser driver could not start")
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
				t.Error("owned editorial browser driver did not join")
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
		t.Fatal("packaged editorial browser workload exceeded 45 seconds")
	}
	if err != nil || stderr.len() != 0 || output.len() > 64<<10 {
		for _, line := range bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n")) {
			var failure struct{ State, Phase, Checkpoint string }
			if json.Unmarshal(line, &failure) == nil && failure.State == "FAIL" && (failure.Phase == "configuration" || failure.Phase == "connect" || failure.Phase == "project-editorial" || failure.Phase == "public-editorial") {
				switch failure.Checkpoint {
				case "", "connect-form", "ordered-content", "canvas-markdown", "layout-1440", "layout-390", "layout-320", "section-anchor-1440", "section-anchor-390", "section-anchor-320", "live-update", "closed-catalogue", "catalogue-detail", "disconnect", "publication-fixture", "public-content", "public-heading", "public-snapshot", "public-order", "public-markdown", "public-links", "public-privacy", "public-layout-1440", "public-layout-390", "public-layout-320":
					t.Logf("editorial safe failure phase=%s checkpoint=%s", failure.Phase, failure.Checkpoint)
				}
			}
		}
		t.Fatalf("packaged editorial browser failed; raw output withheld to protect fixture credentials")
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State                    string `json:"state"`
		EditorialReads           int    `json:"editorial_reads"`
		CompactReads             int    `json:"compact_reads"`
		IndividualReads          int    `json:"individual_reads"`
		External                 int    `json:"external_page_requests"`
		FixtureEntries           int    `json:"fixture_entries"`
		SessionPosts             int    `json:"session_posts"`
		SessionDeletes           int    `json:"session_deletes"`
		DomainWrites             int    `json:"domain_writes"`
		Errors                   int    `json:"errors"`
		PublicationCalls         int    `json:"publication_fixture_calls"`
		AppearanceCalls          int    `json:"appearance_fixture_calls"`
		PublicReads              int    `json:"public_reads"`
		PublicSnapshots          int    `json:"public_snapshots"`
		PublicHistory            int    `json:"public_history"`
		PublicPrivateRequests    int    `json:"public_private_requests"`
		PublicWrites             int    `json:"public_writes"`
		PublicErrors             int    `json:"public_errors"`
		OwnedSnapshotBodies      int    `json:"owned_snapshot_bodies"`
		RetiredSnapshotHandles   int    `json:"retired_snapshot_handles"`
		RetiredSnapshotBodyReads int    `json:"retired_snapshot_body_reads"`
	}
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || result.EditorialReads < 2 || result.CompactReads != 1 || result.IndividualReads != 1 || result.External != 0 || result.FixtureEntries != 3 || result.SessionPosts != 1 || result.SessionDeletes != 1 || result.DomainWrites != 0 || result.Errors != 0 || result.AppearanceCalls != 1 || result.PublicationCalls != 5 || result.PublicReads != 2 || result.PublicSnapshots != 1 || result.PublicHistory != 0 || result.PublicPrivateRequests != 0 || result.PublicWrites != 0 || result.PublicErrors != 0 {
		t.Fatalf("invalid editorial browser result: editorial=%d compact=%d individual=%d external=%d entries=%d", result.EditorialReads, result.CompactReads, result.IndividualReads, result.External, result.FixtureEntries)
	}
	if result.OwnedSnapshotBodies != 1 || result.RetiredSnapshotHandles != 1 || result.RetiredSnapshotBodyReads != 0 || publicWireReads.Load() != 3 || publicWirePrivate.Load() != 0 {
		t.Fatalf("editorial response ownership changed body lifetime, public GET count or anonymous transport: owned=%d retired=%d borrowed=%d wire_reads=%d private_headers=%d", result.OwnedSnapshotBodies, result.RetiredSnapshotHandles, result.RetiredSnapshotBodyReads, publicWireReads.Load(), publicWirePrivate.Load())
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("editorial browser smoke attempted external HTTP")
	}
	if binaryHash(t, binary) != hash {
		t.Fatal("editorial browser smoke changed fcd")
	}
	t.Logf("packaged editorial browser passed: elapsed=%s editorial_reads=%d compact_reads=%d individual_reads=%d external_http=0 browser_domain_writes=0 browser_errors=0 session_posts=1 session_deletes=1 fixture_entries=%d appearance_fixture_calls=1 publication_fixture_calls=5 public_reads=2 public_snapshots=1 public_history=0 public_private_requests=0 public_writes=0 public_errors=0 owned_snapshot_bodies=1 retired_cdp_handles=1 retired_body_reads=0 wire_public_gets=3 wire_private_headers=0 viewports=1440,390,320", time.Since(started).Round(time.Millisecond), result.EditorialReads, result.CompactReads, result.IndividualReads, result.FixtureEntries)
}
