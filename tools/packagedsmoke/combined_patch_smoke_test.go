//go:build packaged_smoke

package packagedsmoke

import (
	"context"
	"encoding/json"
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

func TestPackagedCombinedJobPatchRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private fixture permissions unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute frozen FC_SMOKE_BIN_DIR required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	fc, fcd, fcmcp := filepath.Join(binDir, "fc"+suffix), filepath.Join(binDir, "fcd"+suffix), filepath.Join(binDir, "fcmcp"+suffix)
	if daemonOverride := os.Getenv("FC_RESUME_DAEMON"); daemonOverride != "" {
		if !filepath.IsAbs(daemonOverride) {
			t.Fatal("daemon override must be absolute")
		}
		fcd = daemonOverride
	}
	hashes := map[string][32]byte{fc: binaryHash(t, fc), fcd: binaryHash(t, fcd), fcmcp: binaryHash(t, fcmcp)}
	t.Cleanup(func() {
		for path, hash := range hashes {
			if binaryHash(t, path) != hash {
				t.Error("frozen input changed")
			}
		}
	})
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, fcd, data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("private bootstrap unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, fcd, data, env)
	base := []string{"--profiles-file", filepath.Join(private, "profiles.json"), "--profile", "writer"}
	cli := func(args ...string) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		command := exec.CommandContext(bounded, fc, append(append([]string{}, base...), args...)...)
		command.Env = env
		out, diagnostic := &boundedOutput{}, &boundedOutput{}
		command.Stdout, command.Stderr = out, diagnostic
		if err := command.Run(); err != nil || diagnostic.len() != 0 || !json.Valid(out.bytes()) {
			var failure struct{ Error struct{ Code string } }
			_ = json.Unmarshal(diagnostic.bytes(), &failure)
			t.Fatalf("CLI %s %s failed: error=%v safe_code=%q stdout_bytes=%d stderr_bytes=%d", args[0], args[1], err, failure.Error.Code, out.len(), diagnostic.len())
		}
		return out.bytes()
	}
	cli("profile", "create", "--server", "http://"+daemon.address, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", credential)
	var job client.JobReceipt
	decode(t, cli("jobs", "create", "--title", "Large atomic edit", "--key", "large-create"), &job)
	brief, progress, next := strings.Repeat("<", 32<<10), strings.Repeat("<", 8<<10), strings.Repeat("<", 8<<10)
	file := func(name, body string) string {
		t.Helper()
		path := filepath.Join(private, name)
		if os.WriteFile(path, []byte(body), 0600) != nil {
			t.Fatal("private text fixture unavailable")
		}
		return path
	}
	var receipt client.JobReceipt
	decode(t, cli("jobs", "update", job.ID, "--expect-revision", "1", "--key", "large-atomic-write", "--brief-file", file("brief.md", brief), "--progress-file", file("progress.md", progress), "--next-file", file("next.md", next)), &receipt)
	// Advance separately before recovering the original receipt. Successful replay
	// must preserve both original identity and the newer state.
	cli("jobs", "update", job.ID, "--expect-revision", "2", "--key", "later-title", "--title", "Later title")
	command := exec.CommandContext(ctx, fcmcp, base...)
	command.Env = env
	stderr := &boundedOutput{}
	command.Stderr = stderr
	session, err := mcp.NewClient(&mcp.Implementation{Name: "fresh-large-patch", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatal("actual SDK subprocess connect failed", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil || stderr.len() != 0 || command.ProcessState == nil || !command.ProcessState.Success() {
			t.Error("owned MCP did not join cleanly")
		}
	})
	patch := map[string]any{"job_id": job.ID, "idempotency_key": "large-atomic-write", "expected_revision": 1, "brief": brief, "progress": map[string]any{"summary": progress, "next": next}}
	encoded, _ := json.Marshal(patch)
	if len(encoded) <= 256<<10 || len(encoded) > 384<<10 {
		t.Fatal("fixture does not exercise documented larger encoded patch", len(encoded))
	}
	for _, input := range []struct {
		name string
		args any
	}{{"sdk-map", patch}, {"escaped-json", json.RawMessage(encoded)}} {
		t.Run(input.name, func(t *testing.T) {
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "job_update", Arguments: input.args})
			if err != nil || result == nil || result.IsError {
				code := ""
				if result != nil {
					body, _ := json.Marshal(result.StructuredContent)
					var failure struct{ Error struct{ Code string } }
					_ = json.Unmarshal(body, &failure)
					code = failure.Error.Code
				}
				t.Fatalf("valid exact recovery refused: encoded_bytes=%d transport_error=%v safe_code=%q", len(encoded), err, code)
			}
			body, _ := json.Marshal(result.StructuredContent)
			var replay client.JobReceipt
			if json.Unmarshal(body, &replay) != nil || replay != receipt {
				t.Fatal("exact replay changed original receipt")
			}
		})
	}
	currentResult, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "job_read", Arguments: map[string]any{"job_id": job.ID}})
	if err != nil || currentResult == nil || currentResult.IsError {
		t.Fatal("large authoritative read failed after exact recovery")
	}
	currentBody, _ := json.Marshal(currentResult.StructuredContent)
	var current client.Job
	decode(t, currentBody, &current)
	if current.Revision != 3 || current.Title != "Later title" || current.Brief != brief || current.Progress.Summary != progress || current.Progress.Next != next || external.Load() != 0 {
		t.Fatal("exact replay lost later state, text, or touched external origin")
	}
	t.Logf("actual CLI/service/SDK: encoded_patch=%d bytes original_revision=2 later_revision=3 replay_exact=true external=0", len(encoded))
}
