//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Uses installed build outputs; publication setup remains a deliberate bound
// administrator workflow and never makes anonymous access a private credential.
func TestPackagedPublicationSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private publication fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute packaged binary directory required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	fc, fcd := filepath.Join(binDir, "fc"+suffix), filepath.Join(binDir, "fcd"+suffix)
	fcHash, fcdHash := binaryHash(t, fc), binaryHash(t, fcd)
	defer func() {
		if binaryHash(t, fc) != fcHash || binaryHash(t, fcd) != fcdHash {
			t.Error("packaged inputs changed")
		}
	}()
	var providers atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { providers.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, fcd, data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	token, err := os.ReadFile(adminFile)
	if err != nil {
		t.Fatal("bootstrap token unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(token)), private)
	daemon := startDaemon(t, ctx, fcd, data, env)
	health(t, ctx, daemon.address)
	profiles := filepath.Join(private, "admin.json")
	call := func(args ...string) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		command := exec.CommandContext(bounded, fc, append([]string{"--profiles-file", profiles, "--profile", "admin"}, args...)...)
		command.Env = env
		stdout, stderr := &boundedOutput{}, &boundedOutput{}
		command.Stdout, command.Stderr = stdout, stderr
		if err := command.Run(); err != nil || stderr.len() != 0 {
			var diagnostic struct {
				Code  string `json:"code"`
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(stderr.bytes(), &diagnostic)
			t.Fatalf("packaged publication operation %s failed: %s (%v)", args[0], diagnostic.Code+diagnostic.Error.Code, err)
		}
		out := stdout.bytes()
		if len(out) > 64<<10 || !json.Valid(out) {
			t.Fatal("invalid packaged publication output")
		}

		if bytes.Contains(out, bytes.TrimSpace(token)) {
			t.Fatal("credential disclosed")
		}
		return out
	}
	call("profile", "create", "--server", "http://"+daemon.address, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
	var config struct {
		Revision int64
		Enabled  bool
	}
	decode(t, call("publication", "read"), &config)
	if config.Revision != 0 || config.Enabled {
		t.Fatal("default publication must be private")
	}
	input := filepath.Join(private, "publication.json")
	document := []byte(`{"expected_revision":0,"enabled":true,"manifest":{"slug":"packaged-public","presentation":{"name":"Packaged public project"},"description":"Approved anonymous content"}}`)
	if os.WriteFile(input, document, 0600) != nil {
		t.Fatal("publication file unavailable")
	}
	var preview struct {
		SHA256 string `json:"preview_sha256"`
	}
	decode(t, call("publication", "preview", "--file", input), &preview)
	if len(preview.SHA256) != 64 {
		t.Fatal("preview hash unavailable")
	}
	args := []string{"publication", "apply", "--file", input, "--preview-sha256", preview.SHA256, "--key", "packaged-public-apply"}
	receipt := call(args...)
	if !bytes.Equal(receipt, call(args...)) {
		t.Fatal("exact apply changed receipt")
	}
	publicRead := func(status int) {
		t.Helper()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+daemon.address+"/api/v1/public/projects/packaged-public", nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal("anonymous read unavailable")
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("anonymous status: %d", response.StatusCode)
		}
		if bytes.Contains(body, bytes.TrimSpace(token)) || bytes.Contains(body, []byte(project)) || bytes.Contains(body, []byte(worker)) {
			t.Fatal("anonymous identity disclosure")
		}
		if status == 200 && !bytes.Contains(body, []byte("Approved anonymous content")) {
			t.Fatal("approved content missing")
		}
	}
	publicRead(200)
	withdrawn := call("publication", "unpublish", "--expect-revision", "1", "--key", "packaged-public-withdraw")
	if !bytes.Equal(withdrawn, call("publication", "unpublish", "--expect-revision", "1", "--key", "packaged-public-withdraw")) {
		t.Fatal("withdraw retry changed receipt")
	}
	publicRead(404)
	if !bytes.Equal(receipt, call(args...)) {
		t.Fatal("historical exact apply did not recover original receipt")
	}
	publicRead(404)
	if providers.Load() != 0 {
		t.Fatal("publication called an external provider")
	}
	t.Logf("packaged publication fc=%x fcd=%x; preview/apply/replay/unpublish and anonymous privacy passed", fcHash, fcdHash)
}
