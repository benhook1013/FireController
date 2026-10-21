//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const stepTimeout = 5 * time.Second

// The tag keeps binary execution out of ordinary package tests. This test never
// builds binaries: CI runs it after make check build, using those exact outputs.
func TestPackagedSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private fixture directory unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		var err error
		binDir, err = filepath.Abs(filepath.Join("..", "..", "bin"))
		if err != nil {
			t.Fatal("packaged binary directory unavailable")
		}
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("FC_SMOKE_BIN_DIR must be absolute")
	}
	binaries, hashes := map[string]string{}, map[string][32]byte{}
	for _, name := range []string{"fc", "fcd", "fcmcp"} {
		filename := name
		if runtime.GOOS == "windows" {
			filename += ".exe"
		}
		path := filepath.Join(binDir, filename)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("packaged %s missing; run make build before make smoke", name)
		}
		if _, err := exec.LookPath(path); err != nil {
			t.Fatalf("packaged %s is not executable", name)
		}
		binaries[name] = path
		hashes[name] = binaryHash(t, path)
		t.Logf("packaged input %s sha256=%x", name, hashes[name])
	}

	// Unexpected provider traffic fails locally. The binaries receive neither a
	// catalog nor provider credentials; only loopback application reads/writes
	// are part of the workload. No installed environment/profile is inherited.
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		external.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TEMP=" + private, "TMP=" + private, "PATH=", "FC_PROFILE=unselected-default",
		"HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, binaries["fcd"], data, env)
	health(t, ctx, bootstrap.address)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil || len(bytes.TrimSpace(admin)) == 0 {
		t.Fatal("actual daemon did not provision a private bootstrap credential")
	}
	if info, err := os.Stat(adminFile); err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("bootstrap credential is not private")
	}
	bootstrap.stop(t)

	// Trusted setup is deliberately separate from packaged application proof:
	// use authenticated handlers and a synthetic repository resolver while the
	// real daemon is stopped. No SQL manufactures state or receipts.
	project, worker, otherWorker, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(admin)), private)
	daemon := startDaemon(t, ctx, binaries["fcd"], data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("identity changed after setup")
	}
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var targetMu sync.RWMutex
	proxy.Director = func(r *http.Request) {
		targetMu.RLock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		targetMu.RUnlock()
	}
	proxy.FlushInterval = -1
	var requests, streams, applicationWrites atomic.Int64
	var contactMu sync.Mutex
	var streamClient, jobClient string
	opened, closed := make(chan struct{}, 1), make(chan struct{}, 1)
	proxy.ModifyResponse = func(r *http.Response) error {
		if strings.HasSuffix(r.Request.URL.Path, "/message-events/stream") && r.StatusCode == 200 {
			select {
			case opened <- struct{}{}:
			default:
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) }
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			applicationWrites.Add(1)
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/") {
			contactMu.Lock()
			jobClient = r.Header.Get("X-FC-Session")
			contactMu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/message-events/stream") {
			streams.Add(1)
			contactMu.Lock()
			streamClient = r.Header.Get("X-FC-Session")
			contactMu.Unlock()
			defer func() {
				select {
				case closed <- struct{}{}:
				default:
				}
			}()
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	profileFile, otherProfile := filepath.Join(private, "builder.json"), filepath.Join(private, "inspector.json")
	workerToken, err := os.ReadFile(credential)
	if err != nil {
		t.Fatal("private fixture credential became unavailable")
	}
	privateToken := bytes.TrimSpace(workerToken)
	// Observation bypasses the gateway and carries no worker/session attribution,
	// so it cannot manufacture the very client contact under test.
	contactTransport := &http.Transport{Proxy: nil}
	t.Cleanup(contactTransport.CloseIdleConnections)
	contactHTTP := &http.Client{Transport: contactTransport, Timeout: stepTimeout}
	contactReads := 0
	readContact := func(readCtx context.Context) core.WorkerPresence {
		t.Helper()
		request, err := http.NewRequestWithContext(readCtx, http.MethodGet, "http://"+daemon.address+"/api/v1/projects/"+project+"/workers/presence?worker_ids="+worker+","+otherWorker, nil)
		if err != nil {
			t.Fatal("contact observation request unavailable")
		}
		request.Header.Set("Authorization", "Bearer "+string(privateToken))
		request.Header.Set("X-FC-Instance", daemon.instance)
		response, err := contactHTTP.Do(request)
		if err != nil {
			t.Fatal("packaged contact read unavailable")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
		response.Body.Close()
		contactReads++
		if err != nil || response.StatusCode != http.StatusOK || len(body) > 4096 || bytes.Contains(body, privateToken) || bytes.Contains(body, bytes.TrimSpace(admin)) {
			t.Fatal("invalid or private packaged contact output")
		}
		var value core.WorkerPresence
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&value) != nil || value.ProjectID != project || value.ObservedSince.IsZero() || value.EvaluatedAt.Before(value.ObservedSince) || !value.Complete || len(value.Items) != 2 || value.Items[0].WorkerID != worker || value.Items[1].WorkerID != otherWorker || value.Items[1].Attached {
			t.Fatal("packaged contact scope/order/coverage invalid")
		}
		return value
	}
	contactDetached := func() core.WorkerPresence {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for attempts := 0; attempts < 100; attempts++ {
			value := readContact(bounded)
			if !value.Items[0].Attached {
				if value.Items[0].LastSeenAt == nil {
					t.Fatal("joined client lost last contact")
				}
				return value
			}
			select {
			case <-bounded.Done():
				t.Fatal("joined wait contact did not detach within original step budget")
			case <-tick.C:
			}
		}
		t.Fatal("joined wait exceeded bounded contact observations")
		return core.WorkerPresence{}
	}
	var maxCommand time.Duration
	cli := func(profilePath, name string, args ...string) json.RawMessage {
		t.Helper()
		begin := time.Now()
		result := runCLI(t, ctx, binaries["fc"], env, append([]string{"--profiles-file", profilePath, "--profile", name}, args...)...)
		if bytes.Contains(result, privateToken) || bytes.Contains(result, bytes.TrimSpace(admin)) {
			t.Fatal("packaged CLI exposed a fixture credential")
		}
		if elapsed := time.Since(begin); elapsed > maxCommand {
			maxCommand = elapsed
		}
		return result
	}
	for _, binding := range []struct{ path, name, worker string }{{profileFile, "builder", worker}, {otherProfile, "inspector", otherWorker}} {
		cli(binding.path, binding.name, "profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", binding.worker, "--credential-file", credential)
	}
	var doctor struct {
		Verified  bool   `json:"verified"`
		ProjectID string `json:"project_id"`
		WorkerID  string `json:"worker_id"`
	}
	decode(t, cli(profileFile, "builder", "doctor"), &doctor)
	if !doctor.Verified || doctor.ProjectID != project || doctor.WorkerID != worker {
		t.Fatal("actual CLI doctor did not verify its bound scope")
	}
	jobArgs := []string{"jobs", "create", "--key", "packaged-smoke-create", "--title", "Packaged smoke job", "--brief", "Synthetic packaged smoke brief", "--worker", worker}
	receipt := cli(profileFile, "builder", jobArgs...)
	if !bytes.Equal(receipt, cli(profileFile, "builder", jobArgs...)) {
		t.Fatal("actual CLI exact mutation replay changed receipt")
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
		Outcome  string `json:"outcome"`
	}
	decode(t, receipt, &created)
	if created.ID == "" || created.Revision != 1 || created.Outcome != "created" {
		t.Fatal("invalid job mutation receipt")
	}
	job := cli(profileFile, "builder", "jobs", "read", created.ID)
	checkJob(t, job, created.ID, project, worker)
	var assigned struct {
		Items []json.RawMessage `json:"items"`
	}
	decode(t, cli(otherProfile, "inspector", "jobs", "assigned"), &assigned)
	if assigned.Items == nil || len(assigned.Items) != 0 {
		t.Fatal("worker assignments crossed the second bound profile")
	}

	mcpCommand := exec.CommandContext(ctx, binaries["fcmcp"], "--profiles-file", profileFile, "--profile", "builder")
	mcpCommand.Env = env
	mcpStderr := &boundedOutput{}
	mcpCommand.Stderr = mcpStderr
	sdk := mcp.NewClient(&mcp.Implementation{Name: "packaged-smoke", Version: "1"}, nil)
	connectCtx, stopConnect := context.WithTimeout(ctx, stepTimeout)
	session, err := sdk.Connect(connectCtx, &mcp.CommandTransport{Command: mcpCommand, TerminateDuration: time.Second}, nil)
	stopConnect()
	if err != nil {
		if mcpCommand.Process != nil && mcpCommand.ProcessState == nil {
			_ = mcpCommand.Process.Kill()
			_ = mcpCommand.Wait()
		}
		t.Fatal("official SDK could not initialize actual fcmcp")
	}
	var closeOnce sync.Once
	closeDone := make(chan struct{})
	var closeErr error
	closeMCP := func() {
		closeOnce.Do(func() { go func() { closeErr = session.Close(); close(closeDone) }() })
		select {
		case <-closeDone:
			if closeErr != nil || mcpCommand.ProcessState == nil || !mcpCommand.ProcessState.Success() || mcpStderr.len() != 0 {
				t.Error("actual fcmcp did not shut down cleanly with protocol-only stdout and empty stderr")
			}
		case <-time.After(stepTimeout):
			_ = mcpCommand.Process.Kill()
			select {
			case <-closeDone:
			case <-time.After(time.Second):
				t.Error("owned fcmcp did not join after kill")
			}
			t.Error("actual fcmcp shutdown exceeded five seconds")
		}
	}
	t.Cleanup(closeMCP)
	toolCtx, cancelTools := context.WithTimeout(ctx, stepTimeout)
	defer cancelTools()
	needed := map[string]bool{"job_read": false, "inbox_snapshot": false, "wait_for_messages": false, "workers_list": false, "worker_pause_read": false, "worker_pause_history": false, "worker_pause_replace": false}
	params, seen := &mcp.ListToolsParams{}, map[string]bool{}
	for {
		tools, err := session.ListTools(toolCtx, params)
		if err != nil {
			t.Fatal("actual fcmcp tool discovery failed")
		}
		if params.Cursor == "" {
			if tools.NextCursor != "" {
				t.Fatal("actual fcmcp current inventory must fit the first discovery page")
			}
			firstNames := map[string]bool{}
			for _, tool := range tools.Tools {
				firstNames[tool.Name] = true
			}
			for name := range needed {
				if !firstNames[name] {
					t.Fatalf("critical packaged tool %s absent from first discovery page", name)
				}
			}
		}
		for _, tool := range tools.Tools {
			if _, exists := needed[tool.Name]; exists {
				needed[tool.Name] = true
			}
		}
		if tools.NextCursor == "" {
			break
		}
		if seen[tools.NextCursor] {
			t.Fatal("actual fcmcp discovery repeated a page cursor")
		}
		seen[tools.NextCursor] = true
		params.Cursor = tools.NextCursor
	}
	cancelTools()
	for name, found := range needed {
		if !found {
			t.Fatalf("needed packaged tool %s is missing", name)
		}
	}
	callTool := func(callCtx context.Context, name string, args any) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(callCtx, stepTimeout)
		defer stop()
		value, err := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || value == nil || value.IsError || len(value.Content) != 1 {
			t.Fatalf("actual fcmcp %s failed", name)
		}
		body, err := json.Marshal(value.StructuredContent)
		if err != nil || len(body) > 64<<10 || bytes.Contains(body, privateToken) || bytes.Contains(body, bytes.TrimSpace(admin)) {
			t.Fatal("invalid or oversized MCP structured output")
		}
		text, ok := value.Content[0].(*mcp.TextContent)
		if !ok || !json.Valid([]byte(text.Text)) || !jsonEqual(body, []byte(text.Text)) {
			t.Fatal("MCP compatibility text is not matching JSON")
		}
		return body
	}
	checkJob(t, callTool(ctx, "job_read", map[string]any{"job_id": created.ID}), created.ID, project, worker)
	var snapshot struct {
		EventCursor string `json:"event_cursor"`
		Inbox       struct {
			Items []json.RawMessage `json:"items"`
		} `json:"inbox"`
	}
	decode(t, callTool(ctx, "inbox_snapshot", map[string]any{"limit": 1}), &snapshot)
	if snapshot.EventCursor == "" || snapshot.Inbox.Items == nil || len(snapshot.Inbox.Items) != 0 {
		t.Fatal("actual MCP inbox snapshot was not empty with a replay watermark")
	}
	waitCtx, stopWait := context.WithCancel(ctx)
	t.Cleanup(stopWait)
	type waitResult struct {
		result *mcp.CallToolResult
		err    error
	}
	waitDone := make(chan waitResult, 1)
	go func() {
		result, err := session.CallTool(waitCtx, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"cursor": snapshot.EventCursor, "wait_seconds": 50}})
		waitDone <- waitResult{result, err}
	}()
	select {
	case <-opened:
	case <-time.After(stepTimeout):
		t.Fatal("actual MCP quiet wait did not establish its stream")
	}
	mcpContact := readContact(ctx)
	if !mcpContact.Items[0].Attached || mcpContact.Items[0].LastSeenAt == nil {
		t.Fatal("actual MCP quiet wait did not retain client contact")
	}
	waitWrites := applicationWrites.Load()
	checkJob(t, callTool(ctx, "job_read", map[string]any{"job_id": created.ID}), created.ID, project, worker)
	contactMu.Lock()
	sameClient := streamClient != "" && streamClient == jobClient
	sessionID := streamClient
	contactMu.Unlock()
	if decoded, err := hex.DecodeString(sessionID); !sameClient || err != nil || len(decoded) != 16 || strings.ToLower(sessionID) != sessionID {
		t.Fatal("concurrent MCP tool did not retain its private client connection")
	}
	select {
	case <-waitDone:
		t.Fatal("quiet wait completed without an incoming message or cancellation")
	default:
	}
	stopWait()
	select {
	case result := <-waitDone:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatal("quiet wait cancellation returned a successful result")
		}
	case <-time.After(stepTimeout):
		t.Fatal("quiet wait cancellation did not return promptly")
	}
	select {
	case <-closed:
	case <-time.After(stepTimeout):
		t.Fatal("quiet wait cancellation did not release its HTTP stream")
	}
	if streams.Load() != 1 {
		t.Fatal("quiet wait silently retried or opened extra streams")
	}
	mcpDetached := contactDetached()
	if mcpDetached.Items[0].LastSeenAt.Before(*mcpContact.Items[0].LastSeenAt) || applicationWrites.Load() != waitWrites {
		t.Fatal("MCP wait/contact changed write workload or lost contact")
	}
	checkJob(t, callTool(ctx, "job_read", map[string]any{"job_id": created.ID}), created.ID, project, worker)
	cliWaitCtx, stopCLIWait := context.WithCancel(ctx)
	defer stopCLIWait()
	cliWait := exec.CommandContext(cliWaitCtx, binaries["fc"], "--profiles-file", profileFile, "--profile", "builder", "inbox", "wait", "--cursor", snapshot.EventCursor, "--wait-seconds", "50")
	cliWait.Env = env
	cliWait.WaitDelay = time.Second
	// Signal cancellation exercises fc's real cancellation path on Unix;
	// Windows lacks Interrupt and proves abrupt client disconnect instead.
	if runtime.GOOS != "windows" {
		cliWait.Cancel = func() error { return cliWait.Process.Signal(os.Interrupt) }
	}
	cliOut, cliErr := &boundedOutput{}, &boundedOutput{}
	cliWait.Stdout, cliWait.Stderr = cliOut, cliErr
	if err := cliWait.Start(); err != nil {
		t.Fatal("background packaged CLI wait could not start")
	}
	cliJoined := make(chan struct{})
	var cliWaitErr error
	go func() { cliWaitErr = cliWait.Wait(); close(cliJoined) }()
	t.Cleanup(func() {
		stopCLIWait()
		select {
		case <-cliJoined:
		case <-time.After(stepTimeout):
			_ = cliWait.Process.Kill()
			select {
			case <-cliJoined:
			case <-time.After(time.Second):
				t.Error("owned CLI wait failed to join")
			}
		}
	})
	select {
	case <-opened:
	case <-cliJoined:
		t.Fatal("background CLI wait exited before stream readiness")
	case <-time.After(stepTimeout):
		t.Fatal("background CLI wait did not open within original step budget")
	}
	cliContact := readContact(ctx)
	if !cliContact.Items[0].Attached || cliContact.Items[0].LastSeenAt == nil || cliOut.len() != 0 || cliErr.len() != 0 {
		t.Fatal("actual CLI wait lacked contact or emitted heartbeat noise")
	}
	select {
	case <-cliJoined:
		t.Fatal("quiet CLI wait completed without messages or cancellation")
	default:
	}
	stopCLIWait()
	select {
	case <-cliJoined:
	case <-time.After(stepTimeout):
		t.Fatal("CLI wait cancellation did not join within original step budget")
	}
	if cliWaitErr == nil || cliOut.len() != 0 || cliErr.len() > 64<<10 || bytes.Contains(cliErr.bytes(), privateToken) || bytes.Contains(cliErr.bytes(), bytes.TrimSpace(admin)) {
		t.Fatal("CLI cancellation succeeded unexpectedly or exposed private output")
	}
	if runtime.GOOS != "windows" && (!json.Valid(cliErr.bytes()) || !bytes.Contains(cliErr.bytes(), []byte(`"code":"canceled"`))) {
		t.Fatal("actual CLI wait did not return its bounded cancellation diagnostic")
	}
	select {
	case <-closed:
	case <-time.After(stepTimeout):
		t.Fatal("cancelled CLI did not release its HTTP stream")
	}
	cliDetached := contactDetached()
	if cliDetached.Items[0].LastSeenAt.Before(*cliContact.Items[0].LastSeenAt) || streams.Load() != 2 || applicationWrites.Load() != waitWrites {
		t.Fatal("CLI wait/contact changed stream/write workload or lost contact")
	}
	// One normal update replaces the old narrative/checkpoint choice. Exercise
	// actual CLI and MCP partial writes, then recover the original CLI key after
	// a later MCP revision without creating another history entry.
	progressArgs := []string{"jobs", "update", created.ID, "--expect-revision", "1", "--status", "blocked", "--progress", "Actual packaged progress", "--next", "Continue through MCP", "--blocker", "Synthetic dependency", "--key", "packaged-unified-progress"}
	progressReceipt := cli(profileFile, "builder", progressArgs...)
	var progressResult struct {
		Revision int64  `json:"revision"`
		Outcome  string `json:"outcome"`
	}
	decode(t, progressReceipt, &progressResult)
	if progressResult.Revision != 2 || progressResult.Outcome != "updated" || len(progressReceipt) > 512 {
		t.Fatal("unified CLI update did not return a compact completed result")
	}
	callTool(ctx, "job_update", map[string]any{"job_id": created.ID, "expected_revision": 2, "idempotency_key": "packaged-unified-mcp", "status": "active", "progress": map[string]any{"next": "Ready to resume", "blocker": "", "evidence": []string{"test:packaged"}}})
	if !bytes.Equal(progressReceipt, cli(profileFile, "builder", progressArgs...)) {
		t.Fatal("original update key did not replay after later progress")
	}
	job = cli(profileFile, "builder", "jobs", "read", created.ID)
	var current struct {
		Revision int64 `json:"revision"`
		Progress struct {
			Summary, Next, Blocker string
			Evidence               []string
		} `json:"progress"`
		Checklist struct {
			Total int `json:"total"`
		} `json:"checklist"`
		Notes struct {
			Items []json.RawMessage `json:"items"`
		} `json:"notes"`
	}
	decode(t, job, &current)
	if current.Revision != 3 || current.Progress.Summary != "Actual packaged progress" || current.Progress.Next != "Ready to resume" || current.Progress.Blocker != "" || len(current.Progress.Evidence) != 1 || current.Checklist.Total != 0 || current.Notes.Items == nil {
		t.Fatal("default job read lost current unified working state")
	}
	var fields map[string]json.RawMessage
	decode(t, job, &fields)
	for _, name := range []string{"checkpoint", "inbox", "primary", "pause", "updates"} {
		if _, found := fields[name]; found {
			t.Fatal("default job read expanded unrelated coordination or duplicate progress", name)
		}
	}
	var archive struct {
		Items   []json.RawMessage `json:"items"`
		HasMore bool              `json:"has_more"`
	}
	decode(t, callTool(ctx, "job_progress_history", map[string]any{"job_id": created.ID}), &archive)
	if len(archive.Items) != 2 || archive.HasMore {
		t.Fatal("unified progress history duplicated a replay or lost a change")
	}
	closeMCP()
	daemon.stop(t)

	restarted := startDaemon(t, ctx, binaries["fcd"], data, env)
	if restarted.instance != bootstrap.instance {
		t.Fatal("daemon restart lost durable identity")
	}
	// Preserve the actual profile bytes and move only the fixture forwarding
	// destination, as an external deployment proxy could after process restart.
	newTarget, _ := url.Parse("http://" + restarted.address)
	targetMu.Lock()
	target = newTarget
	targetMu.Unlock()
	if !bytes.Equal(receipt, cli(profileFile, "builder", jobArgs...)) || !bytes.Equal(job, cli(profileFile, "builder", "jobs", "read", created.ID)) {
		t.Fatal("restart lost exact receipt replay or retained job state")
	}
	var jobs struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decode(t, cli(profileFile, "builder", "jobs", "assigned"), &jobs)
	if len(jobs.Items) != 1 || jobs.Items[0].ID != created.ID {
		t.Fatal("restart/replay duplicated or lost the one created job")
	}
	restarted.stop(t)
	if external.Load() != 0 {
		t.Fatal("packaged smoke attempted unexpected external HTTP")
	}
	for name, path := range binaries {
		if binaryHash(t, path) != hashes[name] {
			t.Fatalf("smoke modified packaged %s", name)
		}
	}
	t.Logf("packaged smoke passed: total=%s max_cli=%s application_http=%d streams=%d contact_reads=%d cli_mcp_attached_then_detached=true wait_domain_writes=0 external_http=0 owned_processes=0", time.Since(started).Round(time.Millisecond), maxCommand.Round(time.Millisecond), requests.Load(), streams.Load(), contactReads)
}

// Resolve only our newly allocated fixture root: OS temporary directories may
// use aliases (for example /var on macOS), while daemon data paths deliberately
// reject symlink ancestors. Product path validation remains unchanged.
func privateFixtureDirectory(t *testing.T) string {
	t.Helper()
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("private fixture directory unavailable")
	}
	return private
}

// boundedOutput drains child output without allowing a broken executable to
// retain unbounded logs. Failure reports never echo credential-bearing bytes.
type boundedOutput struct {
	mu       sync.Mutex
	body     bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := (64 << 10) - b.body.Len()
	if len(p) > remaining {
		b.overflow = true
		_, _ = b.body.Write(p[:remaining])
	} else {
		_, _ = b.body.Write(p)
	}
	return len(p), nil
}
func (b *boundedOutput) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.body.Bytes())
}
func (b *boundedOutput) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.overflow {
		return 1 << 30
	}
	return b.body.Len()
}

func runCLI(t *testing.T, ctx context.Context, binary string, env []string, args ...string) json.RawMessage {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	command := exec.CommandContext(bounded, binary, args...)
	command.Env = env
	command.WaitDelay = time.Second
	stdout, stderr := &boundedOutput{}, &boundedOutput{}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil || stderr.len() != 0 || stdout.len() > 64<<10 {
		t.Fatal("packaged CLI failed or emitted unexpected output")
	}
	body := stdout.bytes()
	if !json.Valid(body) {
		t.Fatal("packaged CLI stdout is not one JSON document")
	}
	return body
}

func decode(t *testing.T, body []byte, value any) {
	t.Helper()
	if err := json.Unmarshal(body, value); err != nil {
		t.Fatal("packaged JSON response is invalid")
	}
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
func checkJob(t *testing.T, body []byte, id, project, worker string) {
	t.Helper()
	var job struct {
		ID        string `json:"id"`
		ProjectID string `json:"project_id"`
		WorkerID  string `json:"worker_id"`
		Brief     string `json:"brief"`
		Revision  int64  `json:"revision"`
	}
	decode(t, body, &job)
	if job.ID != id || job.ProjectID != project || job.WorkerID != worker || job.Brief != "Synthetic packaged smoke brief" || job.Revision != 1 {
		t.Fatal("packaged job read lost content, scope or revision")
	}
}
func binaryHash(t *testing.T, path string) [32]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("packaged binary unavailable")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal("packaged binary could not be hashed")
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

// Listening output is parsed while exec owns/drains the pipe. Wait therefore
// joins all copying goroutines; a successful stop also proves no extra stdout.
type listeningRecord struct {
	Event      string `json:"event"`
	Address    string `json:"address"`
	InstanceID string `json:"instance_id"`
}
type listeningOutput struct {
	mu      sync.Mutex
	pending []byte
	record  chan listeningRecord
	seen    bool
	invalid bool
}

func (o *listeningOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen {
		o.invalid = o.invalid || len(p) > 0
		return len(p), nil
	}
	if len(o.pending)+len(p) > 4096 {
		o.invalid = true
		return len(p), nil
	}
	o.pending = append(o.pending, p...)
	if newline := bytes.IndexByte(o.pending, '\n'); newline >= 0 {
		var record listeningRecord
		decoder := json.NewDecoder(bytes.NewReader(o.pending[:newline]))
		decoder.DisallowUnknownFields()
		o.invalid = !json.Valid(o.pending[:newline]) || decoder.Decode(&record) != nil || newline != len(o.pending)-1
		o.seen = true
		o.pending = nil
		o.record <- record
	}
	return len(p), nil
}

type daemonProcess struct {
	command  *exec.Cmd
	output   *listeningOutput
	stderr   *boundedOutput
	done     chan struct{}
	err      error
	instance string
	address  string
	once     sync.Once
}

func startDaemon(t *testing.T, ctx context.Context, binary, data string, env []string, extra ...string) *daemonProcess {
	t.Helper()
	args := append([]string{"--data-dir", data, "--listen", "127.0.0.1:0"}, extra...)
	d := &daemonProcess{command: exec.CommandContext(ctx, binary, args...), output: &listeningOutput{record: make(chan listeningRecord, 1)}, stderr: &boundedOutput{}, done: make(chan struct{})}
	d.command.Env, d.command.Stdout, d.command.Stderr = env, d.output, d.stderr
	d.command.WaitDelay = time.Second
	if err := d.command.Start(); err != nil {
		t.Fatal("packaged daemon could not start")
	}
	go func() { d.err = d.command.Wait(); close(d.done) }()
	t.Cleanup(func() { d.stop(t) })
	select {
	case record := <-d.output.record:
		host, _, err := net.SplitHostPort(record.Address)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || record.Event != "listening" || record.InstanceID == "" {
			t.Fatal("packaged daemon listening record was invalid")
		}
		d.instance, d.address = record.InstanceID, record.Address
	case <-d.done:
		// Report bounded fingerprints, never captured diagnostics or private values.
		diagnostic := d.stderr.bytes()
		t.Fatalf("packaged daemon exited before readiness (exit=%d stderr_bytes=%d stderr_sha256=%x)", d.command.ProcessState.ExitCode(), len(diagnostic), sha256.Sum256(diagnostic))
	case <-time.After(stepTimeout):
		t.Fatal("packaged daemon readiness exceeded five seconds")
	}
	return d
}
func (d *daemonProcess) stop(t *testing.T) {
	t.Helper()
	d.once.Do(func() {
		forcedStop := false
		select {
		case <-d.done:
		default:
			if runtime.GOOS == "windows" {
				// Go cannot send os.Interrupt to another Windows process.
				// Kill terminates this owned daemon with exit1: subsequent
				// reads prove crash/restart recovery, not graceful shutdown.
				if err := d.command.Process.Kill(); err != nil {
					t.Error("owned Windows daemon could not be terminated")
				} else {
					forcedStop = true
					t.Log("Windows daemon stop: forced termination; restart checks prove crash recovery, not graceful shutdown")
				}
			} else {
				_ = d.command.Process.Signal(os.Interrupt)
			}
		}
		select {
		case <-d.done:
		case <-time.After(stepTimeout):
			_ = d.command.Process.Kill()
			select {
			case <-d.done:
			case <-time.After(time.Second):
				t.Error("owned daemon did not exit after kill")
				return
			}
			t.Error("packaged daemon shutdown exceeded five seconds")
		}
		d.output.mu.Lock()
		invalid := d.output.invalid || len(d.output.pending) != 0
		d.output.mu.Unlock()
		validExit := d.err == nil
		if forcedStop {
			var exit *exec.ExitError
			validExit = errors.As(d.err, &exit) && exit.ExitCode() == 1
		}
		if !validExit || invalid || d.stderr.len() != 0 {
			t.Error("packaged daemon shutdown failed or emitted non-listening output")
		}
	})
}
func health(t *testing.T, ctx context.Context, address string) {
	t.Helper()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: stepTimeout}
	for _, path := range []string{"/healthz", "/readyz"} {
		r, err := http.NewRequestWithContext(ctx, "GET", "http://"+address+path, nil)
		if err != nil {
			t.Fatal("health request unavailable")
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("packaged daemon health request failed")
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || len(body) > 4096 || !json.Valid(body) {
			t.Fatal("packaged daemon is not healthy/ready")
		}
	}
}

func seedAccess(t *testing.T, ctx context.Context, data, instance, admin, private string) (string, string, string, string) {
	t.Helper()
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("trusted setup could not open stopped fixture")
	}
	defer store.Close()
	if store.InstanceID() != instance {
		t.Fatal("trusted setup changed daemon identity")
	}
	lookups := 0
	mux := server.New(store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		lookups++
		return server.RepositoryIdentity{ID: 991, FullName: "synthetic/packaged-smoke"}, nil
	}))
	call := func(path string, body any) string {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal("trusted setup JSON failed")
		}
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("X-FC-Instance", instance)
		r.Header.Set("Idempotency-Key", core.NewID("smoke"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var receipt struct {
			ID string `json:"id"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ID == "" {
			t.Fatal("trusted authenticated setup failed")
		}
		return receipt.ID
	}
	project := call("/api/v1/projects", map[string]any{"name": "Packaged smoke", "repository": "synthetic/packaged-smoke"})
	prefix := "/api/v1/projects/" + project
	worker := call(prefix+"/workers", map[string]any{"name": "Builder"})
	other := call(prefix+"/workers", map[string]any{"name": "Inspector"})
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("fixture credential unavailable")
	}
	token := hex.EncodeToString(secret)
	digest := sha256.Sum256([]byte(token))
	call(prefix+"/credentials", map[string]any{"token_sha256": hex.EncodeToString(digest[:]), "permissions": []string{"read", "write"}})
	credential := filepath.Join(private, "worker.token")
	if err := os.WriteFile(credential, []byte(token+"\n"), 0600); err != nil {
		t.Fatal("fixture credential file unavailable")
	}
	if lookups != 1 {
		t.Fatal("trusted setup made unexpected repository lookups")
	}
	t.Log("trusted setup: four authenticated handler calls, one synthetic repository lookup; no provider/network setup")
	return project, worker, other, credential
}
