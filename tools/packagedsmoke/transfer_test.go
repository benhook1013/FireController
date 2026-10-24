//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Every retained record is admitted by actual bounded packaged CLI requests.
// This tests additive transfer, rather than lifting complete-observation limits
// or using a trusted SQL fixture to manufacture an oversized current archive.
func TestPackagedTransferSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private transfer fixture unavailable")
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
			t.Error("packaged transfer inputs changed")
		}
	}()
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
	bootstrap := startDaemon(t, ctx, fcd, data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("transfer bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, _, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, fcd, data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("transfer identity changed")
	}
	health(t, ctx, daemon.address)
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var targetMu sync.RWMutex
	proxy.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	var requests, previews, applies atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews/transfer/preview") {
			previews.Add(1)
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews/transfer/apply") {
			applies.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profiles := filepath.Join(private, "transfer-admin.json")
	var calls, outputBytes int
	var maxCLI time.Duration
	call := func(args ...string) json.RawMessage {
		t.Helper()
		begin := time.Now()
		out := runCLI(t, ctx, fc, env, append([]string{"--profiles-file", profiles, "--profile", "admin"}, args...)...)
		calls++
		outputBytes += len(out)
		if elapsed := time.Since(begin); elapsed > maxCLI {
			maxCLI = elapsed
		}
		if bytes.Contains(out, bytes.TrimSpace(admin)) {
			t.Fatal("transfer CLI disclosed authority")
		}
		return out
	}
	call("profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
	profileBytes, err := os.ReadFile(profiles)
	if err != nil {
		t.Fatal("transfer profile unavailable")
	}
	initial := readUpgradeState(t, ctx, data)
	batchFile := func(batch int, revision int64, start, count int) string {
		t.Helper()
		req := reviews.TransferRequest{Version: 1, ExpectedRevision: &revision, RepositoryID: 991, Publisher: fmt.Sprintf("transfer-batch-%d", batch)}
		for _, source := range []string{"producer-a", "producer-b"} {
			pull := reviews.PullInput{Number: 73, Title: "Retained additive history", State: "merged", CIState: "unknown", Mergeability: "unknown", Runs: []reviews.RunInput{}, Attempts: []reviews.Attempt{}}
			for n := start; n < start+count; n++ {
				run := reviews.RunInput{SourceRunID: fmt.Sprintf("shared-run-%03d", n), Channel: "manual", Scope: "selected prepared history", Outcome: "unknown", CoverageLimits: []string{}, Findings: []reviews.Finding{}}
				attempt := reviews.Attempt{SourceAttemptID: fmt.Sprintf("shared-attempt-%03d", n), Channel: "manual", State: "unknown", SourceRunID: run.SourceRunID}
				if n%2 == 0 {
					run.Outcome = "completed"
					run.Complete = true
					run.Finalized = true
					run.StartedAt = "2026-10-01T01:00:00+01:00"
					run.FinishedAt = "2026-10-01T00:00:01.123456789Z"
					attempt.State = "completed"
					attempt.StartedAt = run.StartedAt
					attempt.FinishedAt = run.FinishedAt
				}
				pull.Runs = append(pull.Runs, run)
				pull.Attempts = append(pull.Attempts, attempt)
			}
			req.Groups = append(req.Groups, reviews.TransferGroup{Source: source, Pulls: []reviews.PullInput{pull}})
		}
		raw, e := json.Marshal(req)
		if e != nil || len(raw) > 256<<10 {
			t.Fatal("bounded transfer fixture invalid")
		}
		file := filepath.Join(private, fmt.Sprintf("transfer-%d.json", batch))
		if os.WriteFile(file, raw, 0600) != nil {
			t.Fatal("private transfer batch unavailable")
		}
		return file
	}
	preview := func(file string, count int64) client.ReviewTransferPreview {
		t.Helper()
		before := readUpgradeState(t, ctx, data)
		var value client.ReviewTransferPreview
		out := call("reviews", "transfer-preview", "--file", file)
		decode(t, out, &value)
		if value.ProjectID != project || value.Counts != (client.ReviewTransferCounts{Pulls: 1, Runs: count, Attempts: count}) || !value.ImportedHistory || value.NativeCoverage || value.Coverage != "partial" || len(out) > 4096 || !reflect.DeepEqual(before, readUpgradeState(t, ctx, data)) {
			t.Fatal("preview was not compact SELECT-only partial admission")
		}
		return value
	}
	applyArgs := func(file, hash, key string) []string {
		return []string{"reviews", "transfer-apply", "--file", file, "--preview", hash, "--key", key}
	}
	firstFile := batchFile(1, 0, 0, 50)
	firstBytes, err := os.ReadFile(firstFile)
	if err != nil {
		t.Fatal("original batch unavailable")
	}
	firstPreview := preview(firstFile, 100)
	originalArgs := applyArgs(firstFile, firstPreview.PreviewSHA256, "packaged-transfer-first")
	// Give the actual child an unwritable stdout descriptor. The apply commits
	// before the CLI reports output_failed; no receipt reaches its caller.
	stdout, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal("closed-output fixture unavailable")
	}
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	command := exec.CommandContext(bounded, fc, append([]string{"--profiles-file", profiles, "--profile", "admin"}, originalArgs...)...)
	command.Env = env
	command.WaitDelay = time.Second
	command.Stdout = stdout
	stderr := &boundedOutput{}
	command.Stderr = stderr
	begin := time.Now()
	runErr := command.Run()
	stop()
	_ = stdout.Close()
	calls++
	if elapsed := time.Since(begin); elapsed > maxCLI {
		maxCLI = elapsed
	}
	var exit *exec.ExitError
	var failure struct {
		Error struct {
			Code           string `json:"code"`
			OutcomeUnknown bool   `json:"outcome_unknown"`
			Action         string `json:"action"`
		} `json:"error"`
	}
	if !errors.As(runErr, &exit) || exit.ExitCode() != cli.ExitConfig || stderr.len() > 4096 || json.Unmarshal(stderr.bytes(), &failure) != nil || failure.Error.Code != "output_failed" || !failure.Error.OutcomeUnknown || !strings.Contains(failure.Error.Action, "exact same file contents") || applies.Load() != 1 || bytes.Contains(stderr.bytes(), bytes.TrimSpace(admin)) {
		t.Fatal("committed stdout failure did not preserve single-shot exact recovery")
	}
	var firstPull client.ReviewPull
	decode(t, call("reviews", "pull", "73"), &firstPull)
	if firstPull.Revision != 1 || firstPull.Pull.RunCount != 100 || firstPull.Pull.AttemptCount != 100 || firstPull.Pull.Complete || firstPull.Source != "transfer-batch-1" {
		t.Fatal("first additive batch missing after stdout loss")
	}
	oldPull := call("reviews", "pull", "73", "--snapshot", firstPull.SnapshotID)
	oldRuns := call("reviews", "runs", "73", "--snapshot", firstPull.SnapshotID)
	oldAttempts := call("reviews", "attempts", "73", "--snapshot", firstPull.SnapshotID)
	var oldRunPage client.ReviewRuns
	var oldAttemptPage client.ReviewAttempts
	decode(t, oldRuns, &oldRunPage)
	decode(t, oldAttempts, &oldAttemptPage)
	if len(oldRunPage.Items) != 5 || !oldRunPage.HasMore || oldRunPage.Cursor == "" || len(oldAttemptPage.Items) != 5 || !oldAttemptPage.HasMore || oldAttemptPage.Cursor == "" {
		t.Fatal("original archive did not retain bounded continuation")
	}
	oldRunsNext := call("reviews", "runs", "73", "--snapshot", firstPull.SnapshotID, "--cursor", oldRunPage.Cursor)
	oldAttemptsNext := call("reviews", "attempts", "73", "--snapshot", firstPull.SnapshotID, "--cursor", oldAttemptPage.Cursor)
	for _, batch := range []struct {
		n            int
		revision     int64
		start, count int
	}{{2, 1, 50, 50}, {3, 2, 100, 1}} {
		file := batchFile(batch.n, batch.revision, batch.start, batch.count)
		p := preview(file, int64(2*batch.count))
		var receipt client.ReviewTransferReceipt
		decode(t, call(applyArgs(file, p.PreviewSHA256, fmt.Sprintf("packaged-transfer-%d", batch.n))...), &receipt)
		if receipt.Revision != batch.revision+1 || receipt.Outcome != "transferred" {
			t.Fatal("later additive batch receipt invalid")
		}
	}
	var latest client.ReviewPull
	decode(t, call("reviews", "pull", "73"), &latest)
	if latest.Revision != 3 || latest.Source != "transfer-batch-3" || latest.ObservedAt != firstPull.ObservedAt || latest.Pull.Complete || latest.Pull.RunCount != 202 || latest.Pull.AttemptCount != 202 {
		t.Fatal("multiple bounded batches lost original history or refreshed unrelated observation time")
	}
	runSeen, attemptSeen := map[string]bool{}, map[string]bool{}
	for _, kind := range []string{"runs", "attempts"} {
		cursor := ""
		pages := 0
		for {
			args := []string{"reviews", kind, "73", "--snapshot", latest.SnapshotID, "--limit", "100"}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			out := call(args...)
			pages++
			hasMore := false
			next := ""
			if kind == "runs" {
				var page client.ReviewRuns
				decode(t, out, &page)
				if page.SnapshotID != latest.SnapshotID || len(page.Items) > 100 {
					t.Fatal("current run page escaped its snapshot/bound")
				}
				for _, run := range page.Items {
					key := run.Source + ":" + run.SourceRunID
					if runSeen[key] || run.Source != "producer-a" && run.Source != "producer-b" {
						t.Fatal("run producer/identity duplicated")
					}
					runSeen[key] = true
					if strings.HasSuffix(run.SourceRunID, "000") && (run.StartedAt != "2026-10-01T01:00:00+01:00" || run.FinishedAt != "2026-10-01T00:00:01.123456789Z") {
						t.Fatal("original run timestamps changed")
					}
					if strings.HasSuffix(run.SourceRunID, "001") && (run.StartedAt != "" || run.FinishedAt != "") {
						t.Fatal("unknown run timestamps invented")
					}
				}
				hasMore, next = page.HasMore, page.Cursor
			} else {
				var page client.ReviewAttempts
				decode(t, out, &page)
				if page.SnapshotID != latest.SnapshotID || len(page.Items) > 100 {
					t.Fatal("current attempt page escaped its snapshot/bound")
				}
				for _, attempt := range page.Items {
					key := attempt.Source + ":" + attempt.SourceAttemptID
					if attemptSeen[key] || attempt.Source != "producer-a" && attempt.Source != "producer-b" {
						t.Fatal("attempt producer/identity duplicated")
					}
					attemptSeen[key] = true
					if strings.HasSuffix(attempt.SourceAttemptID, "000") && (attempt.StartedAt != "2026-10-01T01:00:00+01:00" || attempt.FinishedAt != "2026-10-01T00:00:01.123456789Z") {
						t.Fatal("original attempt timestamps changed")
					}
					if strings.HasSuffix(attempt.SourceAttemptID, "001") && (attempt.StartedAt != "" || attempt.FinishedAt != "" || attempt.DurationSeconds != nil) {
						t.Fatal("unknown attempt timing invented")
					}
				}
				hasMore, next = page.HasMore, page.Cursor
			}
			if !hasMore {
				if next != "" {
					t.Fatal("final page retained cursor")
				}
				break
			}
			if next == "" || next == cursor || pages >= 3 {
				t.Fatal("archive continuation duplicated or escaped bounded workload")
			}
			cursor = next
		}
		if pages != 3 {
			t.Fatal("202 retained records did not require exactly three bounded pages")
		}
	}
	if len(runSeen) != 202 || len(attemptSeen) != 202 || !runSeen["producer-a:shared-run-000"] || !runSeen["producer-b:shared-run-000"] || !attemptSeen["producer-a:shared-attempt-000"] || !attemptSeen["producer-b:shared-attempt-000"] {
		t.Fatal("retained original producer collisions missing")
	}
	daemon.stop(t)
	retained := readUpgradeState(t, ctx, data)
	// Transfers may change only the review read model and their own atomic
	// events/receipts. In particular, executor/capture/coverage authority stays
	// byte-identical, as do credentials, jobs, messages and public publication.
	changed := map[string]bool{}
	for _, name := range []string{"review_snapshots", "review_workspace", "review_pull_versions", "review_pull_memberships", "review_run_versions", "review_run_memberships", "review_attempt_versions", "review_attempt_memberships", "review_current_pulls", "review_current_runs", "review_current_attempts", "review_current_totals", "review_current_attempt_order", "review_current_run_order", "events", "receipts", "sqlite_sequence"} {
		changed[name] = true
	}
	if initial.version != retained.version || !reflect.DeepEqual(initial.metadata, retained.metadata) || len(initial.tables) != len(retained.tables) {
		t.Fatal("transfer changed schema or metadata")
	}
	for name, hash := range initial.tables {
		if !changed[name] && (retained.tables[name] != hash || retained.counts[name] != initial.counts[name]) {
			t.Fatalf("transfer changed unrelated retained table %s", name)
		}
	}
	restarted := startDaemon(t, ctx, fcd, data, env)
	if restarted.instance != bootstrap.instance {
		t.Fatal("transfer restart lost instance")
	}
	health(t, ctx, restarted.address)
	nextTarget, _ := url.Parse("http://" + restarted.address)
	targetMu.Lock()
	target = nextTarget
	targetMu.Unlock()
	if b, e := os.ReadFile(profiles); e != nil || !bytes.Equal(b, profileBytes) {
		t.Fatal("restart changed bound profile")
	}
	if b, e := os.ReadFile(firstFile); e != nil || !bytes.Equal(b, firstBytes) {
		t.Fatal("original selected batch changed")
	}
	beforeReplay := readUpgradeState(t, ctx, data)
	receiptBytes := call(originalArgs...)
	var recovered client.ReviewTransferReceipt
	decode(t, receiptBytes, &recovered)
	if recovered.ID != firstPull.SnapshotID || recovered.Revision != 1 || recovered.Outcome != "transferred" || !bytes.Equal(receiptBytes, call(originalArgs...)) || !reflect.DeepEqual(beforeReplay, readUpgradeState(t, ctx, data)) {
		t.Fatal("lost stdout recovery after advancement/restart changed original outcome or state")
	}
	if !bytes.Equal(oldPull, call("reviews", "pull", "73", "--snapshot", firstPull.SnapshotID)) || !bytes.Equal(oldRuns, call("reviews", "runs", "73", "--snapshot", firstPull.SnapshotID)) || !bytes.Equal(oldAttempts, call("reviews", "attempts", "73", "--snapshot", firstPull.SnapshotID)) || !bytes.Equal(oldRunsNext, call("reviews", "runs", "73", "--snapshot", firstPull.SnapshotID, "--cursor", oldRunPage.Cursor)) || !bytes.Equal(oldAttemptsNext, call("reviews", "attempts", "73", "--snapshot", firstPull.SnapshotID, "--cursor", oldAttemptPage.Cursor)) {
		t.Fatal("transfer/restart rewrote old snapshot bytes or cursors")
	}
	restarted.stop(t)
	if previews.Load() != 3 || applies.Load() != 5 || external.Load() != 0 || !reflect.DeepEqual(retained, readUpgradeState(t, ctx, data)) {
		t.Fatal("transfer retried silently, called provider or changed retained state on restart/read/replay")
	}
	t.Logf("packaged transfer passed total=%s fc=%x fcd=%x cli_calls=%d max_cli=%s output_bytes=%d application_http=%d previews=3 applies=5 retained_runs=202 retained_attempts=202 source_collisions=true stdout_loss_recovered=true native_authority_unchanged=true external_http=0 owned_processes=0", time.Since(started).Round(time.Millisecond), fcHash, fcdHash, calls, maxCLI.Round(time.Millisecond), outputBytes, requests.Load())
}
