//go:build packaged_smoke

package packagedsmoke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise a resumed worker and a coordinating peer through actual executables.
// Assignment attention is distinct from durable messages and recipient handling.
func TestPackagedWorkflowAuditRepairs(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private workflow fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) {
		t.Fatal("absolute frozen executable directory required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	fc, fcd, fcmcp := filepath.Join(binDir, "fc"+suffix), filepath.Join(binDir, "fcd"+suffix), filepath.Join(binDir, "fcmcp"+suffix)
	hashes := map[string][32]byte{fc: binaryHash(t, fc), fcd: binaryHash(t, fcd), fcmcp: binaryHash(t, fcmcp)}
	t.Cleanup(func() {
		for path, hash := range hashes {
			if binaryHash(t, path) != hash {
				t.Error("input executable changed")
			}
		}
	})
	var external, requests, pauseReads atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		external.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	// These commands must work before a profile, credential or service exists.
	for _, args := range [][]string{{"workers", "--help"}, {"workers", "list", "--help"}, {"workers", "pause-state", "--help"}, {"jobs", "primary-set", "--help"}} {
		var help struct {
			Usage string `json:"usage"`
		}
		decode(t, runCLI(t, ctx, fc, env, args...), &help)
		if help.Usage == "" {
			t.Fatal("local coordination discovery lacked operation usage")
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, fcd, data, env)
	token, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("bootstrap fixture unavailable")
	}
	bootstrap.stop(t)
	project, receiver, sender, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(token)), private)
	daemon := startDaemon(t, ctx, fcd, data, env)
	target, _ := url.Parse("http://" + daemon.address)
	var targetMu sync.RWMutex
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{Proxy: nil}
	proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		targetMu.RLock()
		defer targetMu.RUnlock()
		originalDirector(r)
		r.URL.Host = target.Host
	}
	proxy.FlushInterval = -1
	streamReady := make(chan struct{}, 2)
	proxy.ModifyResponse = func(r *http.Response) error {
		if strings.HasSuffix(r.Request.URL.Path, "/message-events/stream") && r.StatusCode == http.StatusOK {
			select {
			case streamReady <- struct{}{}:
			default:
			}
		}
		return nil
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pause") {
			pauseReads.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	base := func(role string) []string {
		return []string{"--profiles-file", filepath.Join(private, role+".json"), "--profile", role}
	}
	call := func(role string, args ...string) json.RawMessage {
		t.Helper()
		count := len(args)
		if count > 2 {
			count = 2
		}
		t.Logf("workflow step: role=%s operation=%s", role, strings.Join(args[:count], " "))
		return runCLI(t, ctx, fc, env, append(base(role), args...)...)
	}
	for role, worker := range map[string]string{"receiver": receiver, "sender": sender} {
		call(role, "profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", credential)
	}
	// Persist the pause through an actual restart before resuming either adapter.
	const pauseReason = "Await the requested human design review"
	call("receiver", "workers", "pause", "--reason", pauseReason, "--expect-revision", "0", "--key", "workflow-pause")
	daemon.stop(t)
	daemon = startDaemon(t, ctx, fcd, data, env)
	if daemon.instance != bootstrap.instance {
		t.Fatal("restart replaced fixture identity")
	}
	newTarget, _ := url.Parse("http://" + daemon.address)
	targetMu.Lock()
	target = newTarget
	targetMu.Unlock()
	transport.CloseIdleConnections()
	type doctor struct {
		Verified bool               `json:"verified"`
		WorkerID string             `json:"worker_id"`
		Pause    client.WorkerPause `json:"worker_pause"`
	}
	verifyDoctor := func(raw []byte, before int64) {
		t.Helper()
		var value doctor
		decode(t, raw, &value)
		if !value.Verified || value.WorkerID != receiver || value.Pause.WorkerID != receiver || value.Pause.Revision != 1 || !value.Pause.Paused || value.Pause.Reason != pauseReason || requests.Load()-before != 2 {
			t.Fatal("doctor lost retained pause or added another domain read")
		}
	}
	beforeDoctor := requests.Load()
	verifyDoctor(call("receiver", "doctor"), beforeDoctor)

	connect := func(role string) (*mcp.ClientSession, func()) {
		t.Helper()
		command := exec.CommandContext(ctx, fcmcp, base(role)...)
		command.Env = env
		diagnostic := &boundedOutput{}
		command.Stderr = diagnostic
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		session, err := mcp.NewClient(&mcp.Implementation{Name: "workflow-audit-smoke", Version: "1"}, nil).Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
		stop()
		if err != nil {
			if command.Process != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
			t.Fatal("actual workflow MCP initialization failed")
		}
		var once sync.Once
		closeMCP := func() {
			once.Do(func() {
				done := make(chan error, 1)
				go func() { done <- session.Close() }()
				select {
				case closeErr := <-done:
					if closeErr != nil || diagnostic.len() != 0 || command.ProcessState == nil || !command.ProcessState.Success() {
						t.Error("owned workflow MCP did not exit cleanly")
					}
				case <-time.After(stepTimeout):
					_ = command.Process.Kill()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("owned workflow MCP did not join after kill")
					}
					t.Error("workflow MCP shutdown exceeded bound")
				}
			})
		}
		t.Cleanup(closeMCP)
		return session, closeMCP
	}
	tool := func(session *mcp.ClientSession, name string, args map[string]any) json.RawMessage {
		t.Helper()
		if args == nil {
			args = map[string]any{}
		}
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		result, err := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("actual workflow MCP operation %s failed", name)
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal("workflow MCP result unavailable")
		}
		return raw
	}
	receiverMCP, closeReceiver := connect("receiver")
	senderMCP, closeSender := connect("sender")
	beforeDoctor = requests.Load()
	verifyDoctor(tool(receiverMCP, "doctor", nil), beforeDoctor)
	// Resume is deliberate; discovery never silently clears a human pause.
	call("receiver", "workers", "resume", "--expect-revision", "1", "--key", "workflow-resume")
	if runtime.GOOS != "windows" {
		unsafe := filepath.Join(private, "unsafe-brief.md")
		if os.WriteFile(unsafe, []byte("PRIVATE_WORKFLOW_FILE"), 0600) != nil || os.Chmod(unsafe, 0644) != nil {
			t.Fatal("unsafe file fixture unavailable")
		}
		before := requests.Load()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		command := exec.CommandContext(bounded, fc, append(base("sender"), "jobs", "create", "--title", "Refused input", "--brief-file", unsafe, "--key", "workflow-unsafe")...)
		command.Env, command.WaitDelay = env, time.Second
		stdout, stderr := &boundedOutput{}, &boundedOutput{}
		command.Stdout, command.Stderr = stdout, stderr
		err := command.Run()
		stop()
		var failure struct {
			Error struct{ Code, Message, Action string } `json:"error"`
		}
		decode(t, stderr.bytes(), &failure)
		if err == nil || stdout.len() != 0 || failure.Error.Code != "invalid_arguments" || !strings.Contains(failure.Error.Message, "unsafe permissions") || !strings.Contains(failure.Error.Action, "0600") || strings.Contains(string(stderr.bytes()), unsafe) || strings.Contains(string(stderr.bytes()), "PRIVATE_WORKFLOW_FILE") || requests.Load() != before {
			t.Fatal("unsafe file did not give classified local preflight refusal")
		}
	}
	var created client.JobReceipt
	decode(t, call("sender", "jobs", "create", "--title", "Inspect the workflow repair", "--brief", "Keep assignment attention and messages independent", "--key", "workflow-job"), &created)
	var snapshot client.InboxSnapshot
	decode(t, call("receiver", "inbox", "snapshot"), &snapshot)
	if snapshot.EventCursor == "" || len(snapshot.Inbox.Items) != 0 {
		t.Fatal("initial inbox snapshot invalid")
	}
	// Observe the established stream rather than sleeping before assignment.
	waitCtx, stopWait := context.WithTimeout(ctx, stepTimeout)
	wait := exec.CommandContext(waitCtx, fc, append(base("receiver"), "inbox", "wait", "--cursor", snapshot.EventCursor, "--wait-seconds", "3")...)
	wait.Env, wait.WaitDelay = env, time.Second
	waitOut, waitErr := &boundedOutput{}, &boundedOutput{}
	wait.Stdout, wait.Stderr = waitOut, waitErr
	if wait.Start() != nil {
		stopWait()
		t.Fatal("actual CLI assignment wait did not start")
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- wait.Wait() }()
	var joinOnce sync.Once
	joinWait := func() {
		joinOnce.Do(func() {
			stopWait()
			select {
			case <-waitDone:
			case <-time.After(stepTimeout):
				_ = wait.Process.Kill()
				select {
				case <-waitDone:
				case <-time.After(time.Second):
					t.Error("owned CLI wait did not join")
				}
				t.Error("owned CLI wait shutdown exceeded bound")
			}
		})
	}
	t.Cleanup(joinWait)
	select {
	case <-streamReady:
	case <-waitCtx.Done():
		t.Fatal("CLI wait did not establish its owned stream")
	}
	call("sender", "jobs", "update", created.ID, "--expect-revision", "1", "--worker", receiver, "--key", "workflow-assign")
	select {
	case err := <-waitDone:
		waitDone <- err
		if err != nil || waitErr.len() != 0 {
			t.Fatal("assignment wait exited unsuccessfully")
		}
	case <-waitCtx.Done():
		t.Fatal("assignment did not reach established CLI wait")
	}
	joinWait()
	var attention client.MessageEventPage
	decode(t, waitOut.bytes(), &attention)
	if attention.TimedOut || len(attention.Items) != 1 || attention.Items[0].Type != "job.assigned" || attention.Items[0].ResourceID != created.ID || attention.Items[0].Payload.RecipientWorkerID != receiver || attention.Items[0].Payload.ConversationID != "" {
		t.Fatal("assignment did not return compact scoped attention")
	}
	var inbox client.Page[client.InboxItem]
	decode(t, call("receiver", "inbox", "list", "--all"), &inbox)
	if len(inbox.Items) != 0 {
		t.Fatal("assignment fabricated a durable message")
	}
	var replay client.MessageEventPage
	decode(t, call("receiver", "inbox", "events", "--cursor", snapshot.EventCursor), &replay)
	if len(replay.Items) != 1 || replay.Items[0].Type != "job.assigned" || replay.Items[0].ResourceID != created.ID {
		t.Fatal("assignment attention was not durably replayable")
	}
	call("receiver", "jobs", "primary-set", created.ID, "--expect-revision", "0", "--key", "workflow-focus")
	noteIDs := map[string]bool{}
	for i := 0; i < 7; i++ {
		role := "sender"
		if i%2 == 0 {
			role = "receiver"
		}
		var note client.NoteReceipt
		decode(t, call(role, "notes", "add", "--job", created.ID, "--body", fmt.Sprintf("Exact job reminder %d", i), "--key", fmt.Sprintf("workflow-note-%d", i)), &note)
		noteIDs[note.ID] = true
	}
	var job client.Job
	decode(t, call("receiver", "jobs", "read", created.ID), &job)
	if len(job.Notes.Items) != 5 || !job.Notes.HasMore || job.Notes.Cursor == "" {
		t.Fatal("embedded exact-job notes did not expose continuation")
	}
	// Reassignment must not change the meaning of a saved exact-job note cursor.
	call("sender", "jobs", "update", created.ID, "--expect-revision", strconv.FormatInt(job.Revision, 10), "--worker", sender, "--key", "workflow-reassign")
	var remaining client.Page[client.NoteSummary]
	decode(t, call("receiver", "notes", "list", "--job", created.ID, "--limit", "5", "--cursor", job.Notes.Cursor), &remaining)
	verifyNotes := func(first, rest client.Page[client.NoteSummary]) {
		t.Helper()
		seen := map[string]bool{}
		for _, n := range append(append([]client.NoteSummary{}, first.Items...), rest.Items...) {
			if !noteIDs[n.ID] || seen[n.ID] || n.JobID != created.ID {
				t.Fatal("exact-job notes duplicated or changed scope")
			}
			seen[n.ID] = true
		}
		if len(seen) != 7 || len(rest.Items) != 2 || rest.HasMore {
			t.Fatal("exact-job continuation lost another worker's reminder")
		}
	}
	verifyNotes(job.Notes, remaining)
	decode(t, tool(receiverMCP, "job_read", map[string]any{"job_id": created.ID}), &job)
	decode(t, tool(receiverMCP, "note_list", map[string]any{"job_id": created.ID, "limit": 5, "cursor": job.Notes.Cursor}), &remaining)
	verifyNotes(job.Notes, remaining)
	decode(t, tool(receiverMCP, "wait_for_messages", map[string]any{"cursor": attention.Cursor, "wait_seconds": 1}), &replay)
	if len(replay.Items) != 1 || replay.Items[0].Type != "job.unassigned" || replay.Items[0].ResourceID != created.ID {
		t.Fatal("reassignment attention was unavailable through actual SDK wait")
	}
	var primary client.PrimaryJob
	decode(t, call("receiver", "jobs", "primary"), &primary)
	if primary.JobID != "" || primary.Revision != 2 {
		t.Fatal("reassignment retained stale focus")
	}
	var sent client.MessageReceipt
	sendArgs := []string{"inbox", "send", "--to", receiver, "--body", "Please inspect the linked workflow job", "--key", "workflow-handoff"}
	decode(t, call("sender", sendArgs...), &sent)
	var same client.MessageReceipt
	decode(t, call("sender", sendArgs...), &same)
	if same != sent {
		t.Fatal("handoff original-key retry created another action")
	}
	decode(t, call("sender", "inbox", "sent", "--newest"), &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0].ID != sent.ID || inbox.Items[0].RecipientWorkerID != receiver || inbox.Items[0].Seen || inbox.Items[0].Acknowledged {
		t.Fatal("new sender process could not rediscover its pending handoff")
	}
	decode(t, tool(senderMCP, "outbox_list", nil), &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0].ID != sent.ID {
		t.Fatal("MCP sender discovery lost handoff")
	}
	var delivery client.MessageDelivery
	decode(t, tool(senderMCP, "inbox_delivery_read", map[string]any{"message_id": sent.ID}), &delivery)
	if delivery.ID != sent.ID || delivery.SenderWorkerID != sender || delivery.RecipientWorkerID != receiver || delivery.Seen || delivery.Acknowledged {
		t.Fatal("sender receipt read invented handling")
	}
	tool(receiverMCP, "inbox_ack", map[string]any{"message_id": sent.ID, "idempotency_key": "workflow-handled"})
	decode(t, call("sender", "inbox", "delivery", sent.ID), &delivery)
	if !delivery.Seen || !delivery.Acknowledged {
		t.Fatal("CLI sender did not observe recipient acknowledgment")
	}
	decode(t, tool(senderMCP, "inbox_delivery_read", map[string]any{"message_id": sent.ID}), &delivery)
	if !delivery.Seen || !delivery.Acknowledged {
		t.Fatal("MCP sender did not observe recipient acknowledgment")
	}
	decode(t, call("sender", "inbox", "sent"), &inbox)
	if len(inbox.Items) != 0 {
		t.Fatal("handled handoff remained in pending sender discovery")
	}
	decode(t, call("sender", "inbox", "sent", "--all"), &inbox)
	if len(inbox.Items) != 1 || !inbox.Items[0].Acknowledged {
		t.Fatal("explicit sender history lost handled handoff")
	}
	closeReceiver()
	closeSender()
	daemon.stop(t)
	if external.Load() != 0 {
		t.Fatal("workflow proof attempted external provider traffic")
	}
	t.Logf("packaged workflow repairs passed: elapsed=%s http_requests=%d doctor_pause_reads=%d assignment_wake=true durable_replay=true fabricated_messages=0 exact_job_notes=7 reassignment_scope_preserved=true sender_recovery=true receipt_handling=true external=0 owned_processes=0", time.Since(started).Round(time.Millisecond), requests.Load(), pauseReads.Load())
}
