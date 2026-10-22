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
	"io"
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
	config := map[string]string{"origin": "http://" + daemon.address, "project": project, "worker": worker, "other_worker": otherWorker, "instance": daemon.instance, "admin_file": adminFile, "credential_file": credentialFile, "executable": executable, "proxy": browserDeny.URL, "progress_file": filepath.Join(private, "browser-progress.json")}
	if directory := os.Getenv("FC_BROWSER_PROOF_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			t.Fatal("FC_BROWSER_PROOF_DIR must be absolute")
		}
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal("private browser diagnostic directory unavailable")
		}
		config["diagnostic_directory"] = directory
		config["progress_file"] = filepath.Join(directory, "browser-progress.json")
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
		lastProgress := safeBrowserProgress(config["progress_file"], time.Now())
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
			joined = true
		case <-time.After(stepTimeout):
		}
		t.Fatalf("packaged browser workload exceeded 45 seconds: last_observed=%s", lastProgress)
	}
	if err != nil || stderr.len() != 0 || output.len() > 64<<10 {
		stage := "before-browser-start"
		if output.browserPID() > 1 {
			stage = "after-browser-start"
		}
		t.Fatalf("packaged browser failed at phase=%s stage=%s stderr_bytes=%d driver_exit=%t last_observed=%s; raw output withheld to protect fixture credentials", output.safePhase(), stage, stderr.len(), err != nil, safeBrowserProgress(config["progress_file"], time.Now()))
	}
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State                    string `json:"state"`
		RepeatedFallbackHash     bool   `json:"repeated_fallback_hash"`
		FixtureWrites            int    `json:"fixture_writes"`
		SessionPosts             int    `json:"session_posts"`
		SessionDeletes           int    `json:"session_deletes"`
		SessionRenewals          int    `json:"session_renewals"`
		MissingSessionRefusals   int    `json:"missing_session_refusals"`
		ProgressAccessRefusals   int    `json:"progress_access_refusals"`
		ActionWrites             int    `json:"action_writes"`
		MetadataReconnect        bool   `json:"metadata_reconnect"`
		InjectedMetadataFailures int    `json:"injected_metadata_failures"`
		InjectedConsoleErrors    int    `json:"injected_console_errors"`
		PresentationReads        int    `json:"presentation_reads"`
		MetadataStreams          int    `json:"metadata_streams"`
		InboxStreams             int    `json:"inbox_streams"`
	}
	if len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || !result.RepeatedFallbackHash || result.FixtureWrites != 25 || result.SessionPosts != 4 || result.SessionDeletes != 4 || result.SessionRenewals != 3 || result.MissingSessionRefusals < 1 || result.ProgressAccessRefusals != 1 || result.ActionWrites != 7 || !result.MetadataReconnect || result.InjectedMetadataFailures != 1 || result.InjectedConsoleErrors < 0 || result.InjectedConsoleErrors > 1 || result.PresentationReads != 4 || result.MetadataStreams < 3 || result.MetadataStreams > 4 || result.InboxStreams < 2 || result.InboxStreams > 3 {
		t.Fatalf("invalid packaged browser result: fixture_writes=%d posts=%d deletes=%d renewals=%d refusals=%d actions=%d metadata=%t injected=%d console=%d reads=%d streams=%d inbox=%d", result.FixtureWrites, result.SessionPosts, result.SessionDeletes, result.SessionRenewals, result.MissingSessionRefusals, result.ActionWrites, result.MetadataReconnect, result.InjectedMetadataFailures, result.InjectedConsoleErrors, result.PresentationReads, result.MetadataStreams, result.InboxStreams)
	}
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("browser smoke attempted external HTTP")
	}
	t.Logf("packaged browser passed: total=%s viewport=390 repeated_fallback_hash=passed fixture_writes=25 session_posts=4 session_deletes=4 session_renewals=3 missing_session_control=passed progress_access_refusal=passed browser_action_writes=7 metadata_reconnect=true injected_metadata_failures=1 reader_presentation_reads=4 reader_metadata_streams=%d reader_inbox_streams=%d daemon_external_http=0 page_external_http=0 blocked_engine_requests=%d owned_processes=0", time.Since(started).Round(time.Millisecond), result.MetadataStreams, result.InboxStreams, engine.Load())
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
			State                    string          `json:"state"`
			Phase                    string          `json:"phase"`
			Checkpoint               string          `json:"checkpoint"`
			FailureClass             string          `json:"failure_class"`
			FailureName              string          `json:"failure_name"`
			HTTPStatus               int             `json:"http_status"`
			AllowWrites              bool            `json:"allow_writes"`
			CanEditPresentation      bool            `json:"can_edit_presentation"`
			ActionWrites             int             `json:"action_writes"`
			External                 int             `json:"external"`
			Posts                    int             `json:"session_posts"`
			Deletes                  int             `json:"session_deletes"`
			Renewals                 int             `json:"session_renewals"`
			Refusals                 int             `json:"terminal_responses"`
			Errors                   int             `json:"errors"`
			Writes                   int             `json:"forbidden_writes"`
			SendStatus               int             `json:"send_status"`
			ReplyStatus              int             `json:"reply_status"`
			Conversation             bool            `json:"conversation_match"`
			PresentationReads        int             `json:"presentation_reads"`
			MetadataStreams          int             `json:"metadata_streams"`
			InboxStreams             int             `json:"inbox_streams"`
			InjectedMetadataFailures int             `json:"injected_metadata_failures"`
			InjectedConsoleErrors    int             `json:"injected_console_errors"`
			ReviewRunReads           int             `json:"run_fetches"`
			PresenceReads            int             `json:"presence_reads"`
			PresenceFinished         int             `json:"presence_finished"`
			PresenceResponses        int             `json:"presence_responses"`
			PresenceHTTPStatus       int             `json:"presence_http_status"`
			PresenceFailed           int             `json:"presence_failed"`
			PresenceHeaderErrors     int             `json:"presence_header_errors"`
			PresenceFailureKind      json.RawMessage `json:"presence_failure_kind"`
			PresenceFailureMS        json.RawMessage `json:"presence_failure_ms"`
			PresenceFailureHeadersMS json.RawMessage `json:"presence_failure_headers_ms"`
			PresenceSignalAborts     json.RawMessage `json:"presence_signal_aborts"`
			PresenceSignalAbortMS    json.RawMessage `json:"presence_signal_abort_ms"`
			DeliveryReads            int             `json:"delivery_reads"`
			JobBriefReads            int             `json:"job_brief_reads"`
			JobArchiveReads          int             `json:"job_archive_reads"`
			ContactHistoryReads      int             `json:"contact_history_reads"`
			FallbackGeometry         struct {
				Top        int  `json:"top"`
				MastBottom int  `json:"mast_bottom"`
				Scroll     int  `json:"scroll"`
				Target     int  `json:"target"`
				Aligned    bool `json:"aligned"`
			} `json:"fallback_geometry"`
		}
		if json.Unmarshal(line, &record) != nil || record.State != "FAIL" {
			continue
		}
		switch record.Phase {
		case "cleanup":
			switch record.Checkpoint {
			case "context-close", "browser-close", "server-close":
				return "cleanup/" + record.Checkpoint
			}
			return "cleanup"
		case "configuration", "launch", "connect", "normal-title-mobile-layout", "fixture-update", "live-update", "metadata-failure", "metadata-reconnect", "metadata-live-update", "live-jobs", "live-workers", "live-notes", "contextual-notes", "live-reviews", "reload", "renewal-activity", "renewal-missing-cookie", "progress-access-ended", "disconnect", "explicit-actions-connect", "explicit-actions-send", "explicit-actions-settings", "explicit-actions-uncertain", "delivery-overview":
			if record.Phase == "connect" {
				allowed := false
				switch record.Checkpoint {
				case "root", "project-input", "credential-input", "submit", "session-response", "session-status", "capabilities-finished", "capabilities-read", "capabilities-check", "heading":
					allowed = true
				}
				if allowed && record.External >= 0 && record.External <= 1000 && record.Errors >= 0 && record.Errors <= 1000 {
					return fmt.Sprintf("connect/%s http_status=%d allow_writes=%t can_edit_presentation=%t errors=%d external=%d", record.Checkpoint, record.HTTPStatus, record.AllowWrites, record.CanEditPresentation, record.Errors, record.External)
				}
			}
			if record.Phase == "explicit-actions-connect" {
				switch record.Checkpoint {
				case "root-url", "root-form", "history-index", "project-input", "credential-input", "permission-control-check", "submit", "session-response", "session-status", "capabilities-finished", "capabilities-read", "capabilities-check", "no-action-writes", "heading", "inbox-button", "inbox-route":
					valid := record.HTTPStatus >= 0 && record.HTTPStatus <= 599
					for _, n := range []int{record.Posts, record.Deletes, record.Renewals, record.Refusals, record.Errors, record.Writes, record.ActionWrites, record.External} {
						valid = valid && n >= 0 && n <= 1000
					}
					if valid {
						return fmt.Sprintf("explicit-actions-connect/%s http_status=%d allow_writes=%t can_edit_presentation=%t posts=%d deletes=%d renewals=%d refusals=%d errors=%d forbidden_writes=%d action_writes=%d external=%d", record.Checkpoint, record.HTTPStatus, record.AllowWrites, record.CanEditPresentation, record.Posts, record.Deletes, record.Renewals, record.Refusals, record.Errors, record.Writes, record.ActionWrites, record.External)
					}
					return record.Phase
				}
				return record.Phase
			}
			if record.Phase == "live-reviews" {
				allowed := false
				switch record.Checkpoint {
				case "reviews-open", "unconfigured", "fallback-observation", "fallback-first-jump", "fallback-scroll-away", "fallback-repeat-jump", "seed-observation", "initial-open-pull", "initial-merged-pull", "updated-observation", "updated-open-pull", "updated-merged-pull", "hosted-run", "merged-retention", "delivery-queue", "queue-heading", "queue-pull", "merge-head", "hosted-summary", "current-round-grants", "current-round-limits-shared", "current-round-close-live", "attempt-reason-compact", "attempt-reason-layout", "attempt-reason-detail", "attempt-reason-expanded", "attempt-reason-reads", "reviews-return", "reviews-heading", "selected-worker", "sanitized-literal", "reviews-url", "queue-jump-visible", "reload-head-spa":
					allowed = true
				}
				valid := allowed
				for _, n := range []int{record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.Errors, record.External, record.Writes, record.ActionWrites, record.ReviewRunReads} {
					valid = valid && n >= 0 && n <= 1000
				}
				if valid {
					class := "driver"
					if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
						class = record.FailureClass
					}
					if strings.HasPrefix(record.Checkpoint, "fallback-") {
						g := record.FallbackGeometry
						return fmt.Sprintf("live-reviews/%s class=%s top=%d mast_bottom=%d scroll=%d target=%d aligned=%t errors=%d external=%d forbidden_writes=%d", record.Checkpoint, class, g.Top, g.MastBottom, g.Scroll, g.Target, g.Aligned, record.Errors, record.External, record.Writes)
					}
					return fmt.Sprintf("live-reviews/%s class=%s run_fetches=%d presentation_reads=%d metadata_streams=%d inbox_streams=%d forbidden_writes=%d action_writes=%d errors=%d external=%d", record.Checkpoint, class, record.ReviewRunReads, record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.Writes, record.ActionWrites, record.Errors, record.External)
				}
			}
			if record.Phase == "live-workers" {
				allowed := false
				switch record.Checkpoint {
				case "selected-builder-before-workers", "workers-nav", "builder-inbox-link", "create-worker", "newcomer-visible", "selection-preserved", "builder-details", "pause-default", "primary-existing", "pause-update", "pause-visible", "primary-update", "primary-link", "selected-builder-after-update":
					allowed = true
				}
				valid := allowed
				for _, n := range []int{record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.Errors, record.External, record.Writes, record.ActionWrites} {
					valid = valid && n >= 0 && n <= 1000
				}
				if valid {
					class := "driver"
					if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
						class = record.FailureClass
					}
					return fmt.Sprintf("live-workers/%s class=%s presentation_reads=%d metadata_streams=%d inbox_streams=%d forbidden_writes=%d action_writes=%d errors=%d external=%d", record.Checkpoint, class, record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.Writes, record.ActionWrites, record.Errors, record.External)
				}
			}
			if record.Phase == "delivery-overview" {
				allowed := false
				switch record.Checkpoint {
				case "initial-snapshot", "delivery-heading", "builder-job", "builder-primary", "inspector-paused", "inspector-pause-reason", "unassigned-job", "status-order", "latest-checkpoint-done", "latest-checkpoint-next", "latest-checkpoint", "old-checkpoint-absent", "overview-request-budget", "lane-count", "private-identifiers-absent", "presence-initial", "presence-hidden", "presence-visible", "presence-visible-response", "presence-visible-content", "layout-capture", "notes-nav-absent", "inbox-nav-click", "inbox-worker-selected", "worker-inbox-workers-nav", "worker-inbox-open", "worker-inbox-history", "worker-inbox-back", "worker-inbox-return", "inbox-heading", "inbox-sidebar", "inbox-readonly-composer", "inbox-readonly-writes", "default-appearance", "default-title", "default-indicator-colors", "default-indicator-contrast":
					allowed = true
				}
				valid := allowed && (record.PresenceHTTPStatus == 0 || record.PresenceHTTPStatus >= 100 && record.PresenceHTTPStatus <= 599)
				// Missing optional observations mean unknown, as do zero durations.
				presenceFailureKind := "none"
				if len(record.PresenceFailureKind) != 0 {
					presenceFailureKind = ""
					valid = valid && json.Unmarshal(record.PresenceFailureKind, &presenceFailureKind) == nil && validPresenceFailureKind(presenceFailureKind)
				}
				var presenceFailureMS, presenceFailureHeadersMS, presenceSignalAborts, presenceSignalAbortMS int
				for _, observation := range []struct {
					raw     json.RawMessage
					value   *int
					maximum int
				}{
					{record.PresenceFailureMS, &presenceFailureMS, 120000},
					{record.PresenceFailureHeadersMS, &presenceFailureHeadersMS, 120000},
					{record.PresenceSignalAborts, &presenceSignalAborts, 1000},
					{record.PresenceSignalAbortMS, &presenceSignalAbortMS, 120000},
				} {
					if len(observation.raw) != 0 {
						valid = valid && !bytes.Equal(bytes.TrimSpace(observation.raw), []byte("null")) && json.Unmarshal(observation.raw, observation.value) == nil && *observation.value >= 0 && *observation.value <= observation.maximum
					}
				}
				for _, n := range []int{record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.Errors, record.External, record.Writes, record.ActionWrites, record.PresenceReads, record.PresenceFinished, record.PresenceResponses, record.PresenceFailed, record.PresenceHeaderErrors, record.DeliveryReads, record.JobBriefReads, record.JobArchiveReads, record.ContactHistoryReads} {
					valid = valid && n >= 0 && n <= 1000
				}
				if valid {
					class := "driver"
					if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
						class = record.FailureClass
					}
					name := record.FailureName
					switch name {
					case "TimeoutError", "Error", "AssertionError", "TypeError", "ReferenceError", "RangeError", "Other":
					default:
						name = "Other"
					}
					result := fmt.Sprintf("delivery-overview/%s class=%s error=%s presentation_reads=%d metadata_streams=%d inbox_streams=%d forbidden_writes=%d action_writes=%d errors=%d external=%d", record.Checkpoint, class, name, record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.Writes, record.ActionWrites, record.Errors, record.External)
					if record.Checkpoint == "presence-initial" || record.Checkpoint == "presence-hidden" || record.Checkpoint == "presence-visible" || record.Checkpoint == "presence-visible-response" || record.Checkpoint == "presence-visible-content" {
						result += fmt.Sprintf(" presence_reads=%d presence_finished=%d presence_responses=%d presence_http_status=%d presence_failed=%d presence_header_errors=%d delivery_reads=%d job_brief_reads=%d job_archive_reads=%d contact_history_reads=%d", record.PresenceReads, record.PresenceFinished, record.PresenceResponses, record.PresenceHTTPStatus, record.PresenceFailed, record.PresenceHeaderErrors, record.DeliveryReads, record.JobBriefReads, record.JobArchiveReads, record.ContactHistoryReads)
						result += fmt.Sprintf(" presence_failure_kind=%s presence_failure_ms=%d presence_failure_headers_ms=%d presence_signal_aborts=%d presence_signal_abort_ms=%d", presenceFailureKind, presenceFailureMS, presenceFailureHeadersMS, presenceSignalAborts, presenceSignalAbortMS)
					}
					return result
				}
			}
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

			if record.Phase == "live-notes" {
				switch record.Checkpoint {
				case "worker-history-link", "worker-history-empty", "worker-history-open", "worker-history-response", "worker-history-filters", "worker-history-phase-filter", "worker-note-create":
					class := "driver"
					if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
						class = record.FailureClass
					}
					return fmt.Sprintf("live-notes/%s class=%s errors=%d forbidden_writes=%d", record.Checkpoint, class, record.Errors, record.Writes)
				}
			}

			if record.Phase == "contextual-notes" {
				switch record.Checkpoint {
				case "none", "job-empty", "job-create", "job-appeared", "job-read", "job-pending-detail", "job-open-history", "job-history-loaded", "job-scope-return", "job-consume", "consumed-history-return", "worker-create", "worker-appeared", "worker-dismiss":
					class := "driver"
					if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
						class = record.FailureClass
					}
					return fmt.Sprintf("contextual-notes/%s class=%s errors=%d forbidden_writes=%d", record.Checkpoint, class, record.Errors, record.Writes)
				}
			}
			if record.Phase == "reload" {
				switch record.Checkpoint {
				case "reload-before", "reload-after-navigation", "reload-project-heading", "reload-queue-held", "reload-hash-retained", "reload-target-absent", "reload-queue-response", "reload-queue-unroute", "reload-delivery-held", "reload-initial-alignment", "reload-delivery-response", "reload-delivery-rendered", "reload-head-position", "reload-heading-visible", "reload-session-renewed", "reload-current-authority", "reload-expiry-persistence", "reload-expiry-evidence", "reload-binding-retained", "reload-expiry-window", "reload-cookie-rotated", "reload-presentation-restored", "reload-sidebar-absent", "reload-connection-live", "reload-metadata-live", "reload-reader-counts":
					class := "driver"
					if record.FailureClass == "timeout" || record.FailureClass == "assertion" {
						class = record.FailureClass
					}
					return fmt.Sprintf("reload/%s class=%s presentation_reads=%d metadata_streams=%d inbox_streams=%d injected_metadata_failures=%d injected_console_errors=%d errors=%d external=%d", record.Checkpoint, class, record.PresentationReads, record.MetadataStreams, record.InboxStreams, record.InjectedMetadataFailures, record.InjectedConsoleErrors, record.Errors, record.External)
				}
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

// Exercise the same helper as the driver with genuine Playwright failures.
// No daemon, application credentials or provider inputs are needed.
func TestBrowserFailureDiagnosticsRetained(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	private := privateFixtureDirectory(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for browser diagnostic proof")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal("Node executable unavailable")
	}
	helper, _ := filepath.Abs("browser-diagnostics.mjs")
	playwright, _ := filepath.Abs("../../webapp/node_modules/@playwright/test/index.mjs")
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("browser cache unavailable")
	}
	browserPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browserPath == "" {
		browserPath = filepath.Join(cache, "ms-playwright")
	}
	executable := os.Getenv("FC_BROWSER_EXECUTABLE")
	if !filepath.IsAbs(browserPath) || executable != "" && !filepath.IsAbs(executable) {
		t.Fatal("browser installation selectors must be absolute")
	}
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	config, err := json.Marshal(map[string]string{"helper": helper, "playwright": playwright, "directory": filepath.Join(private, "diagnostics"), "proxy": deny.URL, "executable": executable})
	if err != nil {
		t.Fatal("browser diagnostic configuration unavailable")
	}
	configFile := filepath.Join(private, "diagnostics.json")
	if os.WriteFile(configFile, config, 0600) != nil {
		t.Fatal("browser diagnostic configuration unavailable")
	}
	const probe = `
import fs from 'node:fs/promises';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
import assert from 'node:assert/strict';
let server, browser, passed=false;
try {
  const config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
  const {classifyBrowserFailure,retainBrowserFailure}=await import(pathToFileURL(config.helper));
  const {chromium,expect}=await import(pathToFileURL(config.playwright));
  server=await chromium.launchServer({headless:true,timeout:5000,proxy:{server:config.proxy},...(config.executable?{executablePath:config.executable}:{}),args:['--disable-background-networking','--no-first-run']});
  console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
  browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});
  const context=await browser.newContext();let requests=0;
  await context.route('**/*',route=>{requests++;return route.abort();});
  const page=await context.newPage();
  const secret='PRIVATE_SENTINEL';
  await page.setContent('<button disabled id="'+secret+'">'+secret+'</button>');
  await page.evaluate(secret=>{location.hash=secret;},secret);
  await context.addCookies([{name:'private',value:secret,url:'https://fixture.invalid'}]);
  let matcher;
  try {await expect(page.locator('#'+secret)).toBeEnabled({timeout:50});} catch(error) {matcher=error;}
  assert.equal(matcher?.matcherResult?.name,'toBeEnabled');
  assert.equal(classifyBrowserFailure(matcher),'assertion');
  assert.equal(classifyBrowserFailure(new assert.AssertionError({message:secret})),'assertion');
  assert.equal(classifyBrowserFailure(new Error('assertion')),'assertion');
  assert.equal(classifyBrowserFailure(new Error(secret)),'driver');
  let timeout;
  try {await page.locator('#absent').waitFor({timeout:50});} catch(error) {timeout=error;}
  assert.equal(timeout?.name,'TimeoutError');
  assert.equal(classifyBrowserFailure(timeout),'timeout');
  await page.close();
  const read=async name=>JSON.parse(await fs.readFile(path.join(config.directory,name),'utf8'));
  const closed={phase:'explicit-actions-uncertain',checkpoint:'retry',action_writes:1};
  let closedSeedSeen=false;
  const closedResult=await retainBrowserFailure({directory:config.directory,filename:'failure.json',snapshot:{...closed},
    collect:async snapshot=>{closedSeedSeen=JSON.stringify(await read('failure.json'))===JSON.stringify(closed);snapshot.count=await page.locator('button').count();},
    screenshot:output=>page.screenshot({path:output,timeout:2000})});
  assert(closedSeedSeen);
  assert.deepEqual(closedResult,{...closed,dom_diagnostics_unavailable:true,screenshot_unavailable:true});
  assert.deepEqual(await read('failure.json'),closedResult);
  const second=await context.newPage();await second.setContent('<button>'+secret+'</button>');
  const seed={phase:'live-reviews',checkpoint:'queue-heading',run_fetches:2};
  let reviewSeedSeen=false,screenshotSeedSeen=false;
  const screenshotResult=await retainBrowserFailure({directory:config.directory,filename:'review-failure.json',snapshot:{...seed},
    collect:async snapshot=>{reviewSeedSeen=JSON.stringify(await read('review-failure.json'))===JSON.stringify(seed);snapshot.button_count=await second.locator('button').count();},
    screenshot:async output=>{screenshotSeedSeen=(await read('review-failure.json')).button_count===1;await second.close();await second.screenshot({path:output,timeout:2000});}});
  assert(reviewSeedSeen&&screenshotSeedSeen&&second.isClosed());
  assert.deepEqual(screenshotResult,{...seed,button_count:1,screenshot_unavailable:true});
  assert.deepEqual(await read('review-failure.json'),screenshotResult);
  const delivery={phase:'delivery-overview',checkpoint:'presence-visible',presence_reads:2};
  const partial=await retainBrowserFailure({directory:config.directory,filename:'delivery-failure.json',snapshot:{...delivery},
    collect:async snapshot=>{snapshot.first_count=1;snapshot.closed_count=await page.locator('button').count();}});
  assert.deepEqual(partial,{...delivery,first_count:1,dom_diagnostics_unavailable:true});
  assert.deepEqual(await read('delivery-failure.json'),partial);
  for(const file of ['failure.json','review-failure.json','delivery-failure.json']) {
    const raw=await fs.readFile(path.join(config.directory,file),'utf8');
    assert(!raw.includes(secret));assert(!raw.includes('about:'));assert(!raw.includes('cookie'));
    assert.equal((await fs.stat(path.join(config.directory,file))).mode&0o777,0o600);
  }
  assert.equal((await fs.stat(config.directory)).mode&0o777,0o700);
  assert.equal(requests,0);
  await context.close();passed=true;
} catch {process.exitCode=1;}
finally {
  try {if(browser) await browser.close();if(server) await server.close();}
  catch {passed=false;process.exitCode=1;}
}
console.log(JSON.stringify({state:passed?'PASS':'FAIL',matcher_assertion:passed,closed_page:passed,screenshot_failure:passed,private_json:passed}));
`
	command := exec.CommandContext(ctx, node, "--input-type=module", "-", configFile)
	command.Stdin = strings.NewReader(probe)
	command.Env = []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "PATH=", "PLAYWRIGHT_BROWSERS_PATH=" + browserPath, "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	command.SysProcAttr, command.WaitDelay = &syscall.SysProcAttr{Setpgid: true}, time.Second
	output, stderr := &browserOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = output, stderr
	if command.Start() != nil {
		t.Fatal("browser diagnostic probe could not start")
	}
	defer func() {
		if pid := output.browserPID(); pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}()
	err = command.Wait()
	lines := bytes.Split(bytes.TrimSpace(output.bytes()), []byte("\n"))
	var result struct {
		State             string `json:"state"`
		MatcherAssertion  bool   `json:"matcher_assertion"`
		ClosedPage        bool   `json:"closed_page"`
		ScreenshotFailure bool   `json:"screenshot_failure"`
		PrivateJSON       bool   `json:"private_json"`
	}
	if err != nil || stderr.len() != 0 || external.Load() != 0 || output.browserPID() < 2 || len(lines) != 2 || json.Unmarshal(lines[1], &result) != nil || result.State != "PASS" || !result.MatcherAssertion || !result.ClosedPage || !result.ScreenshotFailure || !result.PrivateJSON {
		t.Fatal("browser diagnostic failure-path proof failed; raw output withheld")
	}
}

func TestBrowserExplicitConnectDiagnosticsAllowlist(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "safe status and counters",
			body: `{"state":"FAIL","phase":"explicit-actions-connect","checkpoint":"capabilities-check","http_status":200,"allow_writes":true,"can_edit_presentation":true,"session_posts":2,"session_deletes":1,"session_renewals":3,"terminal_responses":0,"errors":0,"forbidden_writes":0,"action_writes":7,"external":0,"url":"PRIVATE_SENTINEL","credential":"PRIVATE_SENTINEL","error":"PRIVATE_SENTINEL"}`,
			want: "explicit-actions-connect/capabilities-check http_status=200 allow_writes=true can_edit_presentation=true posts=2 deletes=1 renewals=3 refusals=0 errors=0 forbidden_writes=0 action_writes=7 external=0",
		},
		{
			name: "checkpoint is fixed allowlist",
			body: `{"state":"FAIL","phase":"explicit-actions-connect","checkpoint":"PRIVATE_SENTINEL","http_status":403,"url":"PRIVATE_SENTINEL"}`,
			want: "explicit-actions-connect",
		},
		{
			name: "invalid counters fall back to phase",
			body: `{"state":"FAIL","phase":"explicit-actions-connect","checkpoint":"session-status","http_status":700,"session_posts":-1}`,
			want: "explicit-actions-connect",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := &browserOutput{}
			_, _ = output.Write(append([]byte(tc.body), '\n'))
			if got := output.safePhase(); got != tc.want || strings.Contains(got, "PRIVATE_SENTINEL") {
				t.Fatalf("unsafe explicit-connect diagnostic: %q", got)
			}
		})
	}
}

// Progress is a private observer record, never a source of response bodies or
// output text. Reuse the terminal formatter's fixed phase/checkpoint vocabulary.
func validPresenceFailureKind(value string) bool {
	return value == "none" || value == "aborted" || value == "connection" || value == "other"
}

func safeBrowserProgress(path string, now time.Time) string {
	const limit = 4096
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "unavailable"
	}
	file, err := os.Open(path)
	if err != nil {
		return "unavailable"
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(raw) > limit {
		return "unavailable"
	}
	var progress struct {
		Phase      string                     `json:"phase"`
		Checkpoint string                     `json:"checkpoint"`
		Operation  string                     `json:"operation"`
		Observed   int64                      `json:"observed_unix_ms"`
		Counters   map[string]json.RawMessage `json:"counters"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&progress) != nil || decoder.Decode(new(any)) != io.EOF || progress.Counters == nil || progress.Observed <= 0 || progress.Observed > now.UnixMilli() {
		return "unavailable"
	}
	age := now.UnixMilli() - progress.Observed
	if age > 60000 {
		return "unavailable"
	}
	switch progress.Operation {
	case "workload", "failure-snapshot", "cleanup":
	default:
		return "unavailable"
	}
	record := map[string]any{"state": "FAIL", "phase": progress.Phase, "checkpoint": progress.Checkpoint}
	for name, raw := range progress.Counters {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return "unavailable"
		}
		switch name {
		case "presence_failure_kind":
			var value string
			if json.Unmarshal(raw, &value) != nil || !validPresenceFailureKind(value) {
				return "unavailable"
			}
			record[name] = value
		case "presence_failure_ms", "presence_failure_headers_ms", "presence_signal_abort_ms":
			var value int
			if json.Unmarshal(raw, &value) != nil || value < 0 || value > 120000 {
				return "unavailable"
			}
			record[name] = value
		case "allow_writes", "can_edit_presentation":
			var value bool
			if json.Unmarshal(raw, &value) != nil {
				return "unavailable"
			}
			record[name] = value
		case "presence_http_status":
			var value int
			if json.Unmarshal(raw, &value) != nil || value != 0 && (value < 100 || value > 599) {
				return "unavailable"
			}
			record[name] = value
		case "session_posts", "session_deletes", "session_renewals", "terminal_responses", "errors", "forbidden_writes", "action_writes", "external", "presentation_reads", "metadata_streams", "inbox_streams", "injected_metadata_failures", "injected_console_errors", "http_status", "presence_reads", "presence_finished", "presence_responses", "presence_failed", "presence_header_errors", "presence_signal_aborts", "delivery_reads", "job_brief_reads", "job_archive_reads", "contact_history_reads":
			var value int
			if json.Unmarshal(raw, &value) != nil || value < 0 || value > 1000 {
				return "unavailable"
			}
			record[name] = value
		default:
			return "unavailable"
		}
	}
	body, _ := json.Marshal(record)
	output := &browserOutput{}
	_, _ = output.Write(append(body, '\n'))
	phase := output.safePhase()
	if phase == "driver-start-or-cleanup" {
		return "unavailable"
	}
	return fmt.Sprintf("%s operation=%s age_ms=%d", phase, progress.Operation, age)
}

func TestBrowserProgressDiagnosticsAllowlist(t *testing.T) {
	now := time.UnixMilli(1000000)
	valid := `{"phase":"connect","checkpoint":"capabilities-read","operation":"workload","observed_unix_ms":999750,"counters":{"http_status":200,"errors":0,"external":0}}`
	for _, tc := range []struct{ name, body, want string }{
		{"current", valid, "connect/capabilities-read http_status=200 allow_writes=false can_edit_presentation=false errors=0 external=0 operation=workload age_ms=250"},
		{"snapshot", strings.ReplaceAll(valid, `"workload"`, `"failure-snapshot"`), "connect/capabilities-read http_status=200 allow_writes=false can_edit_presentation=false errors=0 external=0 operation=failure-snapshot age_ms=250"},
		{"cleanup", `{"phase":"cleanup","checkpoint":"browser-close","operation":"cleanup","observed_unix_ms":999750,"counters":{}}`, "cleanup/browser-close operation=cleanup age_ms=250"},
		{"malformed", "{", "unavailable"},
		{"oversize", strings.Repeat(" ", 4097), "unavailable"},
		{"extra-private-field", strings.Replace(valid, `"counters":`, `"body":"PRIVATE_SENTINEL","counters":`, 1), "unavailable"},
		{"private-phase", strings.Replace(valid, `"connect"`, `"PRIVATE_SENTINEL"`, 1), "unavailable"},
		{"private-checkpoint", strings.Replace(valid, `"capabilities-read"`, `"PRIVATE_SENTINEL"`, 1), "connect operation=workload age_ms=250"},
		{"head-click", `{"phase":"live-reviews","checkpoint":"queue-jump-visible","operation":"workload","observed_unix_ms":999750,"counters":{}}`, "live-reviews/queue-jump-visible class=driver run_fetches=0 presentation_reads=0 metadata_streams=0 inbox_streams=0 forbidden_writes=0 action_writes=0 errors=0 external=0 operation=workload age_ms=250"},
		{"head-reload", `{"phase":"live-reviews","checkpoint":"reload-head-spa","operation":"workload","observed_unix_ms":999750,"counters":{}}`, "live-reviews/reload-head-spa class=driver run_fetches=0 presentation_reads=0 metadata_streams=0 inbox_streams=0 forbidden_writes=0 action_writes=0 errors=0 external=0 operation=workload age_ms=250"},
		{"reload-checkpoint", strings.Replace(strings.Replace(valid, `"connect"`, `"reload"`, 1), `"capabilities-read"`, `"reload-current-authority"`, 1), "reload/reload-current-authority class=driver presentation_reads=0 metadata_streams=0 inbox_streams=0 injected_metadata_failures=0 injected_console_errors=0 errors=0 external=0 operation=workload age_ms=250"},
		{"reload-project-heading", `{"phase":"reload","checkpoint":"reload-project-heading","operation":"workload","observed_unix_ms":999750,"counters":{}}`, "reload/reload-project-heading class=driver presentation_reads=0 metadata_streams=0 inbox_streams=0 injected_metadata_failures=0 injected_console_errors=0 errors=0 external=0 operation=workload age_ms=250"},
		{"reload-queue-held", `{"phase":"reload","checkpoint":"reload-queue-held","operation":"workload","observed_unix_ms":999750,"counters":{}}`, "reload/reload-queue-held class=driver presentation_reads=0 metadata_streams=0 inbox_streams=0 injected_metadata_failures=0 injected_console_errors=0 errors=0 external=0 operation=workload age_ms=250"},
		{"reload-hash-retained", `{"phase":"reload","checkpoint":"reload-hash-retained","operation":"workload","observed_unix_ms":999750,"counters":{}}`, "reload/reload-hash-retained class=driver presentation_reads=0 metadata_streams=0 inbox_streams=0 injected_metadata_failures=0 injected_console_errors=0 errors=0 external=0 operation=workload age_ms=250"},
		{"reload-target-absent", `{"phase":"reload","checkpoint":"reload-target-absent","operation":"workload","observed_unix_ms":999750,"counters":{}}`, "reload/reload-target-absent class=driver presentation_reads=0 metadata_streams=0 inbox_streams=0 injected_metadata_failures=0 injected_console_errors=0 errors=0 external=0 operation=workload age_ms=250"},
		{"reload-private-checkpoint", strings.Replace(strings.Replace(valid, `"connect"`, `"reload"`, 1), `"capabilities-read"`, `"PRIVATE_SENTINEL"`, 1), "reload operation=workload age_ms=250"},
		{"private-operation", strings.Replace(valid, `"workload"`, `"PRIVATE_SENTINEL"`, 1), "unavailable"},
		{"private-counter", strings.Replace(valid, `"errors":0`, `"errors":"PRIVATE_SENTINEL"`, 1), "unavailable"},
		{"null-counter", strings.Replace(valid, `"errors":0`, `"errors":null`, 1), "unavailable"},
		{"unbounded-counter", strings.Replace(valid, `"errors":0`, `"errors":1001`, 1), "unavailable"},
		{"old", strings.Replace(valid, "999750", "1", 1), "unavailable"},
		{"future", strings.Replace(valid, "999750", "1000001", 1), "unavailable"},
		{"trailing", valid + `{}`, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "progress.json")
			if os.WriteFile(path, []byte(tc.body), 0600) != nil {
				t.Fatal("progress fixture")
			}
			got := safeBrowserProgress(path, now)
			if got != tc.want || strings.Contains(got, "PRIVATE_SENTINEL") {
				t.Fatalf("unsafe progress diagnostic: %q", got)
			}
		})
	}
}

func TestBrowserReloadDiagnosticsAllowlist(t *testing.T) {
	for _, checkpoint := range []string{"reload-project-heading", "reload-queue-held", "reload-hash-retained", "reload-target-absent", "reload-queue-response", "reload-queue-unroute", "reload-delivery-held", "reload-initial-alignment", "reload-delivery-response", "reload-delivery-rendered", "reload-head-position"} {
		for _, class := range []string{"timeout", "assertion", "driver", "PRIVATE_SENTINEL"} {
			t.Run(checkpoint+"/"+class, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{"state": "FAIL", "phase": "reload", "checkpoint": checkpoint, "failure_class": class, "body": "PRIVATE_SENTINEL", "url": "PRIVATE_SENTINEL", "credential": "PRIVATE_SENTINEL", "error": "PRIVATE_SENTINEL"})
				if err != nil {
					t.Fatal(err)
				}
				output := &browserOutput{}
				_, _ = output.Write(append(body, '\n'))
				wantClass := class
				if class == "PRIVATE_SENTINEL" {
					wantClass = "driver"
				}
				want := fmt.Sprintf("reload/%s class=%s presentation_reads=0 metadata_streams=0 inbox_streams=0 injected_metadata_failures=0 injected_console_errors=0 errors=0 external=0", checkpoint, wantClass)
				if got := output.safePhase(); got != want || strings.Contains(got, "PRIVATE_SENTINEL") {
					t.Fatalf("unsafe reload diagnostic: %q", got)
				}
			})
		}
	}
}

func TestBrowserSendFailureDiagnosticsAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"status-and-counters", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"reply-binding","failure_class":"assertion","send_status":201,"reply_status":201,"conversation_match":false,"session_posts":2,"session_deletes":1,"session_renewals":3,"errors":0,"forbidden_writes":0,"action_writes":4,"external":0,"body":"PRIVATE_SENTINEL","url":"PRIVATE_SENTINEL","credential":"PRIVATE_SENTINEL"}`, "explicit-actions-send/reply-binding class=assertion send_status=201 reply_status=201 conversation_match=false posts=2 deletes=1 renewals=3 errors=0 forbidden_writes=0 action_writes=4 external=0"},
		{"timeout", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"send-response","failure_class":"timeout"}`, "explicit-actions-send/send-response class=timeout send_status=0 reply_status=0 conversation_match=false posts=0 deletes=0 renewals=0 errors=0 forbidden_writes=0 action_writes=0 external=0"},
		{"unknown-class", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"conversation-open","failure_class":"PRIVATE_SENTINEL"}`, "explicit-actions-send/conversation-open class=driver send_status=0 reply_status=0 conversation_match=false posts=0 deletes=0 renewals=0 errors=0 forbidden_writes=0 action_writes=0 external=0"},
		{"unknown-checkpoint", `{"state":"FAIL","phase":"explicit-actions-send","checkpoint":"PRIVATE_SENTINEL","failure_class":"PRIVATE_SENTINEL"}`, "explicit-actions-send"},
		{"empty-worker-history", `{ "state":"FAIL","phase":"live-notes","checkpoint":"worker-history-response","failure_class":"timeout","body":"PRIVATE_SENTINEL"}`, "live-notes/worker-history-response class=timeout errors=0 forbidden_writes=0"},
		{"contextual-note-selector", `{"state":"FAIL","phase":"contextual-notes","checkpoint":"job-scope-return","failure_class":"driver","body":"PRIVATE_SENTINEL"}`, "contextual-notes/job-scope-return class=driver errors=0 forbidden_writes=0"},
		{"contextual-note-private-checkpoint", `{"state":"FAIL","phase":"contextual-notes","checkpoint":"PRIVATE_SENTINEL","failure_class":"assertion"}`, "contextual-notes"},
		{"reload-private-checkpoint", `{"state":"FAIL","phase":"reload","checkpoint":"PRIVATE_SENTINEL"}`, "reload"},
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

func TestBrowserPresenceDiagnosticsAllowlist(t *testing.T) {
	for _, checkpoint := range []string{"presence-initial", "presence-hidden", "presence-visible", "presence-visible-response", "presence-visible-content"} {
		for _, tc := range []struct {
			name       string
			counter    any
			wantDetail bool
		}{
			{"bounded", 2, true}, {"negative", -1, false}, {"too-large", 1001, false}, {"private", "PRIVATE_SENTINEL", false},
		} {
			t.Run(checkpoint+"/"+tc.name, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{"state": "FAIL", "phase": "delivery-overview", "checkpoint": checkpoint, "failure_class": "assertion", "failure_name": "Error", "presence_reads": tc.counter, "presence_finished": 1, "presence_header_errors": 0, "delivery_reads": 1, "job_brief_reads": 0, "job_archive_reads": 0, "contact_history_reads": 0, "url": "PRIVATE_SENTINEL", "headers": "PRIVATE_SENTINEL"})
				if err != nil {
					t.Fatal("diagnostic fixture")
				}
				output := &browserOutput{}
				_, _ = output.Write(append(body, '\n'))
				got := output.safePhase()
				if strings.Contains(got, "PRIVATE_SENTINEL") {
					t.Fatal("private diagnostic value escaped")
				}
				if tc.wantDetail {
					want := "delivery-overview/" + checkpoint + " class=assertion error=Error presentation_reads=0 metadata_streams=0 inbox_streams=0 forbidden_writes=0 action_writes=0 errors=0 external=0 presence_reads=2 presence_finished=1 presence_responses=0 presence_http_status=0 presence_failed=0 presence_header_errors=0 delivery_reads=1 job_brief_reads=0 job_archive_reads=0 contact_history_reads=0 presence_failure_kind=none presence_failure_ms=0 presence_failure_headers_ms=0 presence_signal_aborts=0 presence_signal_abort_ms=0"
					if got != want {
						t.Fatalf("bounded presence diagnostic differs: %q", got)
					}
				} else if got != "delivery-overview" && got != "driver-start-or-cleanup" {
					t.Fatal("invalid counters exposed detail")
				}
			})
		}
	}
	progress := `{"phase":"delivery-overview","checkpoint":"presence-visible","operation":"failure-snapshot","observed_unix_ms":999750,"counters":{"presence_reads":2,"presence_finished":1,"presence_header_errors":0,"delivery_reads":1,"job_brief_reads":0,"job_archive_reads":0,"contact_history_reads":0}}`
	path := filepath.Join(t.TempDir(), "presence-progress.json")
	if os.WriteFile(path, []byte(progress), 0600) != nil {
		t.Fatal("presence progress fixture")
	}
	got := safeBrowserProgress(path, time.UnixMilli(1000000))
	if !strings.HasPrefix(got, "delivery-overview/presence-visible ") || !strings.Contains(got, " presence_reads=2 presence_finished=1 presence_responses=0 presence_http_status=0 presence_failed=0 presence_header_errors=0 delivery_reads=1 job_brief_reads=0 job_archive_reads=0 contact_history_reads=0 presence_failure_kind=none presence_failure_ms=0 presence_failure_headers_ms=0 presence_signal_aborts=0 presence_signal_abort_ms=0 operation=failure-snapshot age_ms=250") {
		t.Fatal("fixed presence progress was not retained")
	}
}

func TestBrowserPresenceLifecycleDiagnosticsAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		valid       bool
	}{
		{"failure-none", "presence_failure_kind", "none", true},
		{"failure-aborted", "presence_failure_kind", "aborted", true},
		{"failure-connection", "presence_failure_kind", "connection", true},
		{"failure-other", "presence_failure_kind", "other", true},
		{"failure-unknown-label", "presence_failure_kind", "timeout", false},
		{"failure-empty-label", "presence_failure_kind", "", false},
		{"failure-private-label", "presence_failure_kind", "PRIVATE_SENTINEL", false},
		{"failure-null-label", "presence_failure_kind", nil, false},
		{"failure-request-unknown", "presence_failure_ms", 0, true},
		{"failure-request-max", "presence_failure_ms", 120000, true},
		{"failure-request-negative", "presence_failure_ms", -1, false},
		{"failure-request-unbounded", "presence_failure_ms", 120001, false},
		{"failure-request-fraction", "presence_failure_ms", 1.5, false},
		{"failure-request-private", "presence_failure_ms", "PRIVATE_SENTINEL", false},
		{"failure-request-null", "presence_failure_ms", nil, false},
		{"failure-headers-observed", "presence_failure_headers_ms", 4999, true},
		{"failure-headers-negative", "presence_failure_headers_ms", -1, false},
		{"failure-headers-unbounded", "presence_failure_headers_ms", 120001, false},
		{"failure-headers-private", "presence_failure_headers_ms", "PRIVATE_SENTINEL", false},
		{"failure-headers-null", "presence_failure_headers_ms", nil, false},
		{"signal-observed", "presence_signal_aborts", 1, true},
		{"signal-count-max", "presence_signal_aborts", 1000, true},
		{"signal-count-negative", "presence_signal_aborts", -1, false},
		{"signal-count-unbounded", "presence_signal_aborts", 1001, false},
		{"signal-count-private", "presence_signal_aborts", "PRIVATE_SENTINEL", false},
		{"signal-count-null", "presence_signal_aborts", nil, false},
		{"signal-timing-max", "presence_signal_abort_ms", 120000, true},
		{"signal-timing-negative", "presence_signal_abort_ms", -1, false},
		{"signal-timing-unbounded", "presence_signal_abort_ms", 120001, false},
		{"signal-timing-private", "presence_signal_abort_ms", "PRIVATE_SENTINEL", false},
		{"signal-timing-null", "presence_signal_abort_ms", nil, false},
		{"response", "presence_responses", 2, true},
		{"failed", "presence_failed", 1, true},
		{"unknown-status", "presence_http_status", 0, true},
		{"minimum-status", "presence_http_status", 100, true},
		{"maximum-status", "presence_http_status", 599, true},
		{"negative-status", "presence_http_status", -1, false},
		{"below-http-status", "presence_http_status", 99, false},
		{"above-http-status", "presence_http_status", 600, false},
		{"private-status", "presence_http_status", "PRIVATE_SENTINEL", false},
		{"negative-responses", "presence_responses", -1, false},
		{"unbounded-responses", "presence_responses", 1001, false},
		{"negative-failed", "presence_failed", -1, false},
		{"unbounded-failed", "presence_failed", 1001, false},
		{"private-failed", "presence_failed", "PRIVATE_SENTINEL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counters := map[string]any{"presence_reads": 2, "presence_finished": 1, "presence_responses": 1, "presence_http_status": 200, "presence_failed": 0, "presence_failure_kind": "none", "presence_failure_ms": 0, "presence_failure_headers_ms": 0, "presence_signal_aborts": 0, "presence_signal_abort_ms": 0}
			counters[tc.field] = tc.value
			record := map[string]any{"state": "FAIL", "phase": "delivery-overview", "checkpoint": "presence-visible", "url": "PRIVATE_SENTINEL", "failure_text": "PRIVATE_SENTINEL"}
			for name, value := range counters {
				record[name] = value
			}
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			output := &browserOutput{}
			_, _ = output.Write(append(body, '\n'))
			got := output.safePhase()
			if strings.Contains(got, "PRIVATE_SENTINEL") {
				t.Fatal("private terminal diagnostic escaped")
			}
			if tc.valid {
				want := fmt.Sprintf(" presence_reads=2 presence_finished=1 presence_responses=%v presence_http_status=%v presence_failed=%v ", counters["presence_responses"], counters["presence_http_status"], counters["presence_failed"])
				if !strings.Contains(got, want) {
					t.Fatalf("safe lifecycle facts lost: %q", got)
				}
				wantDiscriminators := fmt.Sprintf(" presence_failure_kind=%v presence_failure_ms=%v presence_failure_headers_ms=%v presence_signal_aborts=%v presence_signal_abort_ms=%v", counters["presence_failure_kind"], counters["presence_failure_ms"], counters["presence_failure_headers_ms"], counters["presence_signal_aborts"], counters["presence_signal_abort_ms"])
				if !strings.Contains(got, wantDiscriminators) {
					t.Fatalf("safe failure discriminators lost: %q", got)
				}
			} else if got != "delivery-overview" && got != "driver-start-or-cleanup" {
				t.Fatalf("invalid lifecycle detail accepted: %q", got)
			}
			progress := map[string]any{"phase": "delivery-overview", "checkpoint": "presence-visible", "operation": "failure-snapshot", "observed_unix_ms": 999750, "counters": counters}
			path := filepath.Join(t.TempDir(), "progress.json")
			checkProgress := func(wantValid bool) {
				body, err := json.Marshal(progress)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				value := safeBrowserProgress(path, time.UnixMilli(1000000))
				if strings.Contains(value, "PRIVATE_SENTINEL") || wantValid && !strings.Contains(value, "presence_responses=") || !wantValid && value != "unavailable" {
					t.Fatalf("unsafe or missing progress facts: %q", value)
				}
			}
			checkProgress(tc.valid)
			counters["failure_text"] = "PRIVATE_SENTINEL"
			checkProgress(false)
		})
	}
}
