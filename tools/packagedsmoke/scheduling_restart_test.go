//go:build packaged_smoke

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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Uses actual packaged transports and daemon processes. The stopped fixture
// setup creates identities only; scheduling state is never manufactured by SQL.
func TestPackagedSchedulingPauseRestartResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private fixture unavailable")
	}
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if bin == "" {
		var err error
		bin, err = filepath.Abs("../../bin")
		if err != nil {
			t.Fatal(err)
		}
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("absolute packaged binary directory required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binaries, hashes := map[string]string{}, map[string][32]byte{}
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		path := filepath.Join(bin, name+suffix)
		if _, err := exec.LookPath(path); err != nil {
			t.Fatalf("packaged %s unavailable", name)
		}
		binaries[name], hashes[name] = path, binaryHash(t, path)
		t.Logf("scheduling input %s sha256=%x", name, hashes[name])
	}
	t.Cleanup(func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Errorf("packaged %s changed", name)
			}
		}
	})
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	profile := ""
	generation := 0
	bind := func() {
		t.Helper()
		generation++
		profile = filepath.Join(private, fmt.Sprintf("scheduling-%d.json", generation))
		if daemon.instance != bootstrap.instance {
			t.Fatal("restart changed instance")
		}
		health(t, ctx, daemon.address)
		runCLI(t, ctx, binaries["fc"], env, "--profiles-file", profile, "--profile", "scheduling", "profile", "create", "--server", "http://"+daemon.address, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", credential)
	}
	cli := func(args ...string) json.RawMessage {
		t.Helper()
		return runCLI(t, ctx, binaries["fc"], env, append([]string{"--profiles-file", profile, "--profile", "scheduling"}, args...)...)
	}
	bind()
	var initial client.ReviewPullScheduling
	decode(t, cli("reviews", "scheduling", "read", "--pr", "42"), &initial)
	if initial.ProjectID != project || initial.PullNumber != 42 || initial.Revision != 0 || initial.Paused {
		t.Fatal("initial scheduling identity/state")
	}
	command := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profile, "--profile", "scheduling")
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "scheduling-restart-smoke", Version: "1"}, nil)
	session, err := sdk.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatal("MCP connect failed")
	}
	t.Cleanup(func() { _ = session.Close() })
	call := func(name string, args any) json.RawMessage {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("MCP %s failed", name)
		}
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				return json.RawMessage(text.Text)
			}
		}
		t.Fatal("MCP text absent")
		return nil
	}
	pauseArgs := map[string]any{"pull_number": 42, "paused": true, "reason": "Synthetic scheduling hold", "expected_revision": 0, "idempotency_key": "packaged-pause"}
	pausedReceipt := call("review_pull_scheduling_update", pauseArgs)
	if !bytes.Equal(pausedReceipt, call("review_pull_scheduling_update", pauseArgs)) {
		t.Fatal("MCP exact replay changed")
	}
	var paused client.ReviewPullScheduling
	decode(t, call("review_pull_scheduling_read", map[string]any{"pull_number": 42}), &paused)
	if paused.ProjectID != project || paused.PullNumber != 42 || !paused.Paused || paused.Revision != 1 || paused.Reason != "Synthetic scheduling hold" {
		t.Fatal("MCP pause identity/state")
	}
	if err := session.Close(); err != nil || stderr.len() != 0 {
		t.Fatal("MCP shutdown failed")
	}
	daemon.stop(t)
	daemon = startDaemon(t, ctx, binaries["fcd"], data, env)
	bind()
	var retained client.ReviewPullScheduling
	decode(t, cli("reviews", "scheduling", "read", "--pr", "42"), &retained)
	if retained != paused {
		t.Fatal("restart changed paused control")
	}
	resumeArgs := []string{"reviews", "scheduling", "resume", "--pr", "42", "--expect-revision", "1", "--key", "packaged-resume"}
	resumedReceipt := cli(resumeArgs...)
	var resumed client.ReviewPullScheduling
	decode(t, cli("reviews", "scheduling", "read", "--pr", "42"), &resumed)
	if resumed.ProjectID != project || resumed.PullNumber != 42 || resumed.Paused || resumed.Revision != 2 || resumed.Reason != "" {
		t.Fatal("CLI resume identity/state")
	}
	daemon.stop(t)
	daemon = startDaemon(t, ctx, binaries["fcd"], data, env)
	bind()
	decode(t, cli("reviews", "scheduling", "read", "--pr", "42"), &retained)
	if retained != resumed {
		t.Fatal("second restart changed resumed control")
	}
	if !bytes.Equal(resumedReceipt, cli(resumeArgs...)) {
		t.Fatal("restart changed exact resume replay")
	}
	var scheduling client.ReviewScheduling
	decode(t, cli("reviews", "scheduling"), &scheduling)
	if scheduling.ProjectID != project || scheduling.Revision != 0 || scheduling.AutomaticHosted || scheduling.AutomaticCLI {
		t.Fatal("pause/resume changed default-off project controls")
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("scheduling caused external network work")
	}
}
