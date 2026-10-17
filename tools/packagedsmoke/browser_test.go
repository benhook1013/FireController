//go:build packaged_smoke && linux

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
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
	project, worker, otherWorker, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	credentialFile := seedBrowserReadOnlyCredential(t, ctx, data, bootstrap.instance, project, strings.TrimSpace(string(admin)), private)
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
	if directory := os.Getenv("FC_BROWSER_PROOF_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			t.Fatal("FC_BROWSER_PROOF_DIR must be absolute")
		}
		config["diagnostic_directory"] = directory
	}
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
		SessionRenewals          int    `json:"session_renewals"`
		MissingSessionRefusals   int    `json:"missing_session_refusals"`
		ActionWrites             int    `json:"action_writes"`
		MetadataReconnect        bool   `json:"metadata_reconnect"`
		InjectedMetadataFailures int    `json:"injected_metadata_failures"`
		InjectedConsoleErrors    int    `json:"injected_console_errors"`
		PresentationReads        int    `json:"presentation_reads"`
		MetadataStreams          int    `json:"metadata_streams"`
		InboxStreams             int    `json:"inbox_streams"`
	}
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || result.FixtureWrites != 15 || result.SessionPosts != 3 || result.SessionDeletes != 3 || result.SessionRenewals != 3 || result.MissingSessionRefusals < 1 || result.ActionWrites != 7 || !result.MetadataReconnect || result.InjectedMetadataFailures != 1 || result.InjectedConsoleErrors < 0 || result.InjectedConsoleErrors > 1 || result.PresentationReads != 4 || result.MetadataStreams < 3 || result.MetadataStreams > 4 || result.InboxStreams < 2 || result.InboxStreams > 3 {
		t.Fatalf("invalid packaged browser result: fixture_writes=%d posts=%d deletes=%d renewals=%d refusals=%d actions=%d metadata=%t injected=%d console=%d reads=%d streams=%d inbox=%d", result.FixtureWrites, result.SessionPosts, result.SessionDeletes, result.SessionRenewals, result.MissingSessionRefusals, result.ActionWrites, result.MetadataReconnect, result.InjectedMetadataFailures, result.InjectedConsoleErrors, result.PresentationReads, result.MetadataStreams, result.InboxStreams)
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("browser smoke attempted external HTTP")
	}
	t.Logf("packaged browser passed: total=%s viewport=390 fixture_writes=15 session_posts=3 session_deletes=3 session_renewals=3 missing_session_control=passed browser_action_writes=7 metadata_reconnect=true injected_metadata_failures=1 reader_presentation_reads=4 reader_metadata_streams=%d reader_inbox_streams=%d daemon_external_http=0 page_external_http=0 blocked_engine_requests=%d owned_processes=0", time.Since(started).Round(time.Millisecond), result.MetadataStreams, result.InboxStreams, engine.Load())
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
			FailureClass             string `json:"failure_class"`
			ActionWrites             int    `json:"action_writes"`
			External                 int    `json:"external"`
			Posts                    int    `json:"session_posts"`
			Deletes                  int    `json:"session_deletes"`
			Renewals                 int    `json:"session_renewals"`
			Refusals                 int    `json:"terminal_responses"`
			Errors                   int    `json:"errors"`
			Writes                   int    `json:"forbidden_writes"`
			SendStatus               int    `json:"send_status"`
			ReplyStatus              int    `json:"reply_status"`
			Conversation             bool   `json:"conversation_match"`
			PresentationReads        int    `json:"presentation_reads"`
			MetadataStreams          int    `json:"metadata_streams"`
			InboxStreams             int    `json:"inbox_streams"`
			InjectedMetadataFailures int    `json:"injected_metadata_failures"`
			InjectedConsoleErrors    int    `json:"injected_console_errors"`
		}
		if json.Unmarshal(line, &record) != nil || record.State != "FAIL" {
			continue
		}
		switch record.Phase {
		case "configuration", "launch", "connect", "fixture-update", "live-update", "metadata-failure", "metadata-reconnect", "metadata-live-update", "live-jobs", "live-workers", "live-notes", "live-reviews", "reload", "renewal-activity", "renewal-missing-cookie", "disconnect", "explicit-actions-connect", "explicit-actions-send", "explicit-actions-settings", "explicit-actions-uncertain":
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

			if record.Phase == "reload" {
				return fmt.Sprintf("reload/%s presentation_reads=%d metadata_streams=%d inbox_streams=%d injected_metadata_failures=%d injected_console_errors=%d errors=%d external=%d", record.Checkpoint, record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.InjectedMetadataFailures, record.InjectedConsoleErrors, record.Errors, record.External)
			}

			if record.Phase == "explicit-actions-send" {
				class := "driver"
				if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
					class = record.FailureClass
				}
				switch record.Checkpoint {
				case "composer-open", "recipient", "send-body", "send-dispatch", "send-response", "send-status", "conversation-open", "conversation-body", "reply-open", "reply-body", "reply-dispatch", "reply-response", "reply-status", "reply-binding", "reply-visible":
					return fmt.Sprintf("%s/%s class=%s send_status=%d reply_status=%d conversation_match=%t posts=%d deletes=%d renewals=%d errors=%d forbidden_writes=%d action_writes=%d external=%d", record.Phase, record.Checkpoint, class, record.SendStatus, record.ReplyStatus, record.Conversation, record.Posts, record.Deletes, record.Renewals, record.Errors, record.Writes, record.ActionWrites, record.External)
				}
			}
			if record.Phase == "explicit-actions-uncertain" {
				class := "driver"
				if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
					class = record.FailureClass
				}
				switch record.Checkpoint {
				case "compose", "dispatch", "switch-view", "retry-ready", "navigation-guard", "retry-response", "retry-identity", "conversation", "conversation-link", "conversation-body", "disconnect", "action-counters":
					return fmt.Sprintf("%s/%s class=%s posts=%d deletes=%d renewals=%d errors=%d forbidden_writes=%d action_writes=%d external=%d", record.Phase, record.Checkpoint, class, record.Posts, record.Deletes, record.Renewals, record.Errors, record.Writes, record.ActionWrites, record.External)
				}
			}
			if record.Phase == "renewal-missing-cookie" {
				switch record.Checkpoint {
				case "connect", "root-form", "project-input", "credential-input", "submit", "heading", "binding", "remove-cookie", "reload", "cleanup", "storage", "counters":
					return fmt.Sprintf("%s/%s posts=%d deletes=%d renewals=%d refusals=%d errors=%d forbidden_writes=%d", record.Phase, record.Checkpoint, record.Posts, record.Deletes, record.Renewals, record.Refusals, record.Errors, record.Writes)
				}
			}
			return record.Phase
		}
	}
	return "driver-start-or-cleanup"
}

func TestBrowserSendFailureDiagnosticsAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"status-and-counters", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"reply-binding","failure_class":"assertion","send_status":201,"reply_status":201,"conversation_match":false,"session_posts":2,"session_deletes":1,"session_renewals":3,"errors":0,"forbidden_writes":0,"action_writes":4,"external":0,"body":"PRIVATE_SENTINEL","url":"PRIVATE_SENTINEL","credential":"PRIVATE_SENTINEL"}`, "explicit-actions-send/reply-binding class=assertion send_status=201 reply_status=201 conversation_match=false posts=2 deletes=1 renewals=3 errors=0 forbidden_writes=0 action_writes=4 external=0"},
		{"timeout", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"send-response","failure_class":"timeout"}`, "explicit-actions-send/send-response class=timeout send_status=0 reply_status=0 conversation_match=false posts=0 deletes=0 renewals=0 errors=0 forbidden_writes=0 action_writes=0 external=0"},
		{"unknown-class", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"conversation-open","failure_class":"PRIVATE_SENTINEL"}`, "explicit-actions-send/conversation-open class=driver send_status=0 reply_status=0 conversation_match=false posts=0 deletes=0 renewals=0 errors=0 forbidden_writes=0 action_writes=0 external=0"},
		{"unknown-checkpoint", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"PRIVATE_SENTINEL","failure_class":"PRIVATE_SENTINEL"}`, "explicit-actions-send"},
		{"unknown-phase", `{"state":"FAIL","phase":"PRIVATE_SENTINEL","checkpoint":"send-status"}`, "driver-start-or-cleanup"},
		{"string-status", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"send-status","send_status":"PRIVATE_SENTINEL"}`, "driver-start-or-cleanup"},
		{"numeric-match", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"reply-binding","conversation_match":1}`, "driver-start-or-cleanup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := &browserOutput{}
			_, _ = output.Write([]byte(tc.body + "\n"))
			if got := output.safePhase(); got != tc.want || strings.Contains(got, "PRIVATE_SENTINEL") {
				t.Fatal("browser failure diagnostics did not preserve the fixed safe boundary")
			}
		})
	}
}

// Give the browser a genuine read-only credential rather than asking a writer
// session to pretend to be read-only. This setup runs before the actual daemon.
func seedBrowserReadOnlyCredential(t *testing.T, ctx context.Context, data, instance, project, admin, private string) string {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("read-only browser fixture unavailable")
	}
	defer store.Close()
	if store.InstanceID() != instance {
		t.Fatal("read-only browser fixture identity changed")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("read-only browser credential unavailable")
	}
	token := hex.EncodeToString(secret)
	digest := sha256.Sum256([]byte(token))
	raw, err := json.Marshal(map[string]any{"token_sha256": hex.EncodeToString(digest[:]), "permissions": []string{"read"}})
	if err != nil {
		t.Fatal("read-only browser fixture JSON unavailable")
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project+"/credentials", bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+admin)
	request.Header.Set("X-FC-Instance", instance)
	request.Header.Set("Idempotency-Key", core.NewID("browser_readonly"))
	response := httptest.NewRecorder()
	server.New(store).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatal("read-only browser credential setup refused")
	}
	filename := filepath.Join(private, "browser-readonly.token")
	if os.WriteFile(filename, []byte(token+"\n"), 0600) != nil {
		t.Fatal("private read-only browser credential unavailable")
	}
	return filename
}
