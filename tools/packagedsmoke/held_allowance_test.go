//go:build packaged_smoke

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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Trusted fixture setup records an exhaustion observation through its owning
// writer, without manufacturing SQL bodies. Actual packaged clients then read
// it through the daemon before and after restart. No review catalog is installed
// and the fixture cannot execute a provider action.
func TestPackagedHeldAllowanceSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private held fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute packaged binaries required")
	}
	binaries := map[string]string{}
	hashes := map[string][32]byte{}
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		filename := name
		if runtime.GOOS == "windows" {
			filename += ".exe"
		}
		binaries[name] = filepath.Join(binDir, filename)
		hashes[name] = binaryHash(t, binaries[name])
	}
	t.Cleanup(func() {
		for name, path := range binaries {
			if binaryHash(t, path) != hashes[name] {
				t.Errorf("packaged %s changed", name)
			}
		}
	})
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	admin, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap authority unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	operation, observation := seedHeldAllowance(t, ctx, data, project, worker, strings.TrimSpace(string(admin)))
	before := readUpgradeState(t, ctx, data)
	calls, mcpCalls := 0, 0
	var expected []byte
	for pass := 0; pass < 2; pass++ {
		daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
		if daemon.instance != bootstrap.instance {
			t.Fatal("restart changed identity")
		}
		// A separate private profile binds this process address; no existing profile
		// is overwritten or implicitly selected by another client.
		profile := "held-before"
		if pass == 1 {
			profile = "held-after"
		}
		profiles := filepath.Join(private, profile+".json")
		call := func(args ...string) []byte {
			t.Helper()
			bounded, stop := context.WithTimeout(ctx, stepTimeout)
			defer stop()
			command := exec.CommandContext(bounded, binaries["fc"], append([]string{"--profiles-file", profiles, "--profile", profile}, args...)...)
			command.Env = env
			command.WaitDelay = time.Second
			stdout, stderr := &boundedOutput{}, &boundedOutput{}
			command.Stdout, command.Stderr = stdout, stderr
			if err := command.Run(); err != nil || stderr.len() != 0 || stdout.len() > 64<<10 {
				var failure struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				_ = json.Unmarshal(stderr.bytes(), &failure)
				code := failure.Error.Code
				if len(code) > 64 || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz_") != "" {
					code = "unclassified"
				}
				t.Fatalf("held CLI step %s failed (code=%s, pass=%d)", args[0], code, pass)
			}
			raw := stdout.bytes()
			calls++
			if len(raw) > 64<<10 || bytes.Contains(raw, bytes.TrimSpace(admin)) {
				t.Fatal("private or unbounded output")
			}
			return raw
		}
		call("profile", "create", "--server", "http://"+daemon.address, "--instance", bootstrap.instance, "--project", project, "--worker", worker, "--credential-file", credential)
		raw := call("reviews", "operation", operation, "--wait-seconds", "50", "--diagnostics")
		var value client.ReviewOperation
		decode(t, raw, &value)
		if value.ID != operation || value.State != "held" || value.Reason != "allowance_exhausted" || value.Allowance == nil {
			t.Fatal("held allowance absent")
		}
		fact := value.Allowance
		if fact.Kind != "cli_exhausted" || fact.ObservedAt != observation.ObservedAt || fact.Remaining == nil || *fact.Remaining != 0 || fact.Limit == nil || *fact.Limit != observation.Limit || fact.FullCapacityInMS == nil || *fact.FullCapacityInMS != 0 || fact.RollingWindowMS == nil || *fact.RollingWindowMS != observation.RollingWindowMS || fact.RetryNotBefore != "" {
			t.Fatal("original known-zero facts changed")
		}
		if expected == nil {
			expected = bytes.Clone(raw)
		} else if !jsonEqual(expected, raw) {
			t.Fatal("restart changed retained operation")
		}
		command := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profiles, "--profile", profile)
		command.Env = env
		stderr := &boundedOutput{}
		command.Stderr = stderr
		sdk := mcp.NewClient(&mcp.Implementation{Name: "held-allowance-smoke", Version: "1"}, nil)
		connectCtx, stop := context.WithTimeout(ctx, stepTimeout)
		session, err := sdk.Connect(connectCtx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
		stop()
		if err != nil {
			if command.Process != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
			t.Fatal("actual SDK initialization failed")
		}
		toolCtx, stopTool := context.WithTimeout(ctx, stepTimeout)
		result, toolErr := session.CallTool(toolCtx, &mcp.CallToolParams{Name: "review_operation_read", Arguments: map[string]any{"operation_id": operation, "wait_seconds": 50, "diagnostics": true}})
		stopTool()
		mcpCalls++
		closeErr := session.Close()
		if closeErr != nil || command.ProcessState == nil || !command.ProcessState.Success() || stderr.len() != 0 {
			t.Fatal("MCP did not join cleanly")
		}
		if toolErr != nil || result == nil || result.IsError || len(result.Content) != 1 {
			t.Fatal("MCP held read failed")
		}
		structured, err := json.Marshal(result.StructuredContent)
		text, ok := result.Content[0].(*mcp.TextContent)
		if err != nil || !ok || !jsonEqual(raw, structured) || !jsonEqual(raw, []byte(text.Text)) {
			t.Fatal("CLI and MCP retained facts differ")
		}
		daemon.stop(t)
		after := readUpgradeState(t, ctx, data)
		for name, hash := range before.tables {
			if strings.HasPrefix(name, "review_") && (after.tables[name] != hash || after.counts[name] != before.counts[name]) {
				t.Fatalf("read/restart changed %s", name)
			}
		}
	}
	if external.Load() != 0 {
		t.Fatal("held reads attempted external HTTP")
	}
	t.Logf("held allowance packaged proof passed: total=%s cli=%d mcp=%d restarts=1 known_zero_preserved=true review_rows_unchanged=true external_http=0 owned_processes=0", time.Since(started).Round(time.Millisecond), calls, mcpCalls)
}

func seedHeldAllowance(t *testing.T, ctx context.Context, data, project, worker, admin string) (string, reviewexec.CLIAllowanceObservation) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped held fixture unavailable")
	}
	defer store.Close()
	engine, err := reviewexec.New(store, []reviewexec.Capability{{ID: "held-proof", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal("fixture engine unavailable")
	}
	request := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	request.SetPathValue("project_id", project)
	request.Header.Set("Authorization", "Bearer "+admin)
	request.Header.Set("X-FC-Instance", store.InstanceID())
	request.Header.Set("X-FC-Worker", worker)
	scope, err := store.AdminScope(request)
	if err != nil {
		t.Fatal("fixture scope unavailable")
	}
	zero := int64(0)
	if _, err = engine.Configure(ctx, scope, "held-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "held-proof", Mode: "independent", Queue: []int64{1}, Channels: []string{"cli"}}); err != nil {
		t.Fatal("fixture configuration rejected", err)
	}
	if _, err = engine.ConfigurePolicy(ctx, scope, "held-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"cli"}}); err != nil {
		t.Fatal("fixture policy rejected", err)
	}
	raw, err := engine.Enqueue(ctx, scope, "held-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 1, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal("fixture request rejected", err)
	}
	var receipt reviewexec.Receipt
	decode(t, raw, &receipt)
	attempt, err := engine.StartAttempt(ctx, project, receipt.ID)
	if err != nil {
		t.Fatal("fixture preparing attempt rejected", err)
	}
	observed := time.Now().UTC().Format(time.RFC3339Nano)
	input := reviewexec.CLIAllowanceObservation{Anchor: reviewexec.Anchor{RepositoryID: 991, Repository: "synthetic/packaged-smoke", PullNumber: 1, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), BaseRef: "main", MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("c", 64), ObservedAt: observed}, ObservedAt: observed, Remaining: 0, Limit: 3, FullCapacityInMS: 0, RollingWindowMS: 3600000, ChangedFiles: 1}
	if err = engine.RecordCLIExhaustion(ctx, project, attempt.ID, attempt.Revision, input); err != nil {
		t.Fatal("fixture exhaustion rejected", err)
	}
	if _, err = engine.Advance(ctx, project, attempt.ID, attempt.Revision, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal("fixture hold rejected", err)
	}
	if err = engine.RecordCLIExhaustion(ctx, project, attempt.ID, attempt.Revision, input); err != nil {
		t.Fatal("exact original observation replay rejected", err)
	}
	return receipt.ID, input
}
