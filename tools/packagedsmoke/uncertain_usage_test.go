//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/benhook1013/FireController/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Lose actual committed responses, discard the calling process, restart the
// daemon, and continue from private handoff material through fresh clients.
// This exercises the documented recovery workflow, not a retrying transport.
func TestPackagedUncertainUsageRecovery(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if os.Chmod(private, 0700) != nil {
		t.Fatal("private recovery fixture unavailable")
	}
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if bin == "" {
		bin, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("absolute frozen executable directory required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	fc, fcd, fcmcp := filepath.Join(bin, "fc"+suffix), filepath.Join(bin, "fcd"+suffix), filepath.Join(bin, "fcmcp"+suffix)
	hashes := map[string][32]byte{fc: binaryHash(t, fc), fcd: binaryHash(t, fcd), fcmcp: binaryHash(t, fcmcp)}
	t.Cleanup(func() {
		for file, hash := range hashes {
			if binaryHash(t, file) != hash {
				t.Error("frozen executable changed")
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
	daemon := startDaemon(t, ctx, fcd, data, env)
	adminFile := filepath.Join(data, "bootstrap-admin.token")
	admin, err := os.ReadFile(adminFile)
	if err != nil {
		t.Fatal("fixture bootstrap unavailable")
	}
	daemon.stop(t)
	project, worker, recipient, _ := seedAccess(t, ctx, data, daemon.instance, strings.TrimSpace(string(admin)), private)
	daemon = startDaemon(t, ctx, fcd, data, env)
	instance := daemon.instance
	target, _ := url.Parse("http://" + daemon.address)
	var mu sync.Mutex
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(r *http.Request) {
		mu.Lock()
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		mu.Unlock()
	}
	// Loss occurs only after the daemon returned its successful committed receipt.
	losses := map[string][]byte{"uncertain-message": nil, "uncertain-progress": nil, "uncertain-item": nil}
	requests := map[string]int{}
	captured := func(key string) []byte {
		mu.Lock()
		defer mu.Unlock()
		return bytes.Clone(losses[key])
	}
	proxy.ModifyResponse = func(r *http.Response) error {
		key := r.Request.Header.Get("Idempotency-Key")
		mu.Lock()
		defer mu.Unlock()
		requests[key]++
		if receipt, lose := losses[key]; lose && receipt == nil {
			body, e := io.ReadAll(io.LimitReader(r.Body, 4097))
			r.Body.Close()
			if e != nil || r.StatusCode < 200 || r.StatusCode > 299 || len(body) > 4096 || !json.Valid(body) {
				t.Error("selected response did not commit a bounded receipt")
			}
			losses[key] = body
			return errors.New("owned response-loss injection")
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		connection, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			_ = connection.Close()
		}
	}
	gateway := httptest.NewServer(proxy)
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	base := []string{"--profiles-file", filepath.Join(private, "builder.json"), "--profile", "builder"}
	call := func(args ...string) json.RawMessage {
		t.Helper()
		return runCLI(t, ctx, fc, env, append(append([]string{}, base...), args...)...)
	}
	call("profile", "create", "--server", gateway.URL, "--instance", instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(private, name)
		if os.WriteFile(path, []byte(body), 0600) != nil {
			t.Fatal("private handoff input unavailable")
		}
		return path
	}
	refuse := func(unknown bool, wantCode string, args ...string) {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		command := exec.CommandContext(bounded, fc, append(append([]string{}, base...), args...)...)
		command.Env, command.WaitDelay = env, time.Second
		out, diagnostic := &boundedOutput{}, &boundedOutput{}
		command.Stdout, command.Stderr = out, diagnostic
		var envelope struct {
			Error struct {
				Code, Action   string
				OutcomeUnknown bool `json:"outcome_unknown"`
			} `json:"error"`
		}
		runErr := command.Run()
		decodeErr := json.Unmarshal(diagnostic.bytes(), &envelope)
		value := envelope.Error
		if runErr == nil || out.len() != 0 || diagnostic.len() > 4096 || decodeErr != nil || value.OutcomeUnknown != unknown || value.Code != wantCode || value.Action == "" || bytes.Contains(diagnostic.bytes(), bytes.TrimSpace(admin)) {
			t.Fatalf("uncertain/refused result lost safe recovery meaning: success=%t out=%d diagnostic=%d json=%t code=%q unknown=%t", runErr == nil, out.len(), diagnostic.len(), decodeErr == nil, value.Code, value.OutcomeUnknown)
		}
		if unknown && (!strings.Contains(value.Action, "key") || !strings.Contains(value.Action, "request")) {
			t.Fatal("uncertain diagnostic omitted same-request/key recovery")
		}
	}
	restart := func() {
		t.Helper()
		daemon.stop(t)
		daemon = startDaemon(t, ctx, fcd, data, env)
		if daemon.instance != instance {
			t.Fatal("restart replaced bound identity")
		}
		fresh, _ := url.Parse("http://" + daemon.address)
		mu.Lock()
		target = fresh
		mu.Unlock()
	}
	// Every invocation here connects a new MCP process; no SDK memory from the
	// lost CLI invocation or earlier recovery can supply a receipt or retry key.
	freshTool := func(name string, args any) json.RawMessage {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		command := exec.CommandContext(bounded, fcmcp, base...)
		command.Env = env
		diagnostic := &boundedOutput{}
		command.Stderr = diagnostic
		session, e := mcp.NewClient(&mcp.Implementation{Name: "uncertain-handoff", Version: "1"}, nil).Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
		if e != nil {
			t.Fatal("fresh MCP connection failed")
		}
		defer session.Close()
		result, e := session.CallTool(bounded, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil || result == nil || result.IsError {
			t.Fatal("fresh MCP recovery failed")
		}
		body, e := json.Marshal(result.StructuredContent)
		if e != nil || bytes.Contains(body, bytes.TrimSpace(admin)) || diagnostic.len() != 0 {
			t.Fatal("fresh MCP output invalid/private")
		}
		if session.Close() != nil || command.ProcessState == nil || !command.ProcessState.Success() {
			t.Fatal("fresh MCP did not join cleanly")
		}
		return body
	}
	text := "  # Handoff π\n\n" + strings.Repeat("Exact retained request. ", 120) + "\n"
	messageFile := write("message.md", text)
	messageArgs := []string{"inbox", "send", "--to", recipient, "--body-file", messageFile, "--key", "uncertain-message"}
	// A handoff retains the invocation and binding. Files are immutable request
	// inputs, rather than inferred from a latest mutable working document.
	handoff, _ := json.Marshal(struct{ Binding, Args []string }{base, messageArgs})
	handoffPath := write("recover-message.json", string(handoff))
	refuse(true, "connection_failed", messageArgs...)
	mu.Lock()
	attempts := requests["uncertain-message"]
	mu.Unlock()
	if attempts != 1 {
		t.Fatal("client silently retried uncertain send")
	}
	restart()
	recovered := freshTool("inbox_send", map[string]any{"recipient_worker_id": recipient, "body": text, "idempotency_key": "uncertain-message"})
	if !jsonEqual(recovered, captured("uncertain-message")) {
		t.Fatal("cross-client handoff changed committed message identity")
	}
	// A changed input must fail closed with the same key; restoring the retained
	// input then recovers the original action, rather than making another send.
	write("message.md", text+"changed")
	refuse(false, "idempotency_conflict", messageArgs...)
	write("message.md", text)
	var resumed struct{ Binding, Args []string }
	retained, e := os.ReadFile(handoffPath)
	if e != nil || json.Unmarshal(retained, &resumed) != nil || !bytes.Equal(handoff, retained) {
		t.Fatal("private handoff changed")
	}
	if !jsonEqual(recovered, runCLI(t, ctx, fc, env, append(resumed.Binding, resumed.Args...)...)) {
		t.Fatal("retained handoff could not recover original receipt")
	}
	var message client.MessageReceipt
	decode(t, recovered, &message)
	var conversation client.ConversationPage
	decode(t, call("inbox", "thread", message.ConversationID), &conversation)
	if len(conversation.Items) != 1 || conversation.HasMore || conversation.Items[0].ID != message.ID || conversation.Items[0].Body != text {
		t.Fatal("response loss/recovery duplicated or changed message")
	}
	var created client.JobReceipt
	decode(t, call("jobs", "create", "--title", "Uncertain handoff job", "--brief", "Retain task definition", "--worker", worker, "--key", "create-job"), &created)
	progressFile := write("progress.md", "Completed meaningful step π\n")
	progressArgs := []string{"jobs", "update", created.ID, "--expect-revision", "1", "--progress-file", progressFile, "--next", "Continue checklist", "--key", "uncertain-progress"}
	refuse(true, "connection_failed", progressArgs...)
	restart()
	// Another client advances current state before the old uncertain action is
	// recovered. Recovery returns its historical receipt, preserving newer work.
	freshTool("job_update", map[string]any{"job_id": created.ID, "expected_revision": 2, "progress": map[string]any{"next": "Newer next step"}, "idempotency_key": "newer-progress"})
	if !jsonEqual(call(progressArgs...), captured("uncertain-progress")) {
		t.Fatal("old progress recovery lost original receipt")
	}
	items := []client.ChecklistItem{{ID: "selected", Text: "One useful step"}, {ID: "later", Text: "Retain other work"}}
	itemJSON, _ := json.Marshal(items)
	call("jobs", "checklist-set", created.ID, "--expect-revision", "3", "--expect-checklist-revision", "0", "--items-file", write("items.json", string(itemJSON)), "--key", "set-plan")
	itemArgs := []string{"jobs", "checklist-item", created.ID, "selected", "--done", "true", "--expect-checklist-revision", "1", "--key", "uncertain-item"}
	refuse(true, "connection_failed", itemArgs...)
	restart()
	if !jsonEqual(freshTool("job_checklist_item_update", map[string]any{"job_id": created.ID, "item_id": "selected", "done": true, "expected_revision": 1, "idempotency_key": "uncertain-item"}), captured("uncertain-item")) {
		t.Fatal("item recovery lost receipt")
	}
	var job client.Job
	decode(t, freshTool("job_read", map[string]any{"job_id": created.ID}), &job)
	if job.Revision != 3 || job.Progress.Summary != "Completed meaningful step π\n" || job.Progress.Next != "Newer next step" || job.Checklist.Revision != 2 || job.Checklist.Completed != 1 || len(job.Checklist.Remaining) != 1 || job.Checklist.Remaining[0].ID != "later" || job.Brief != "Retain task definition" {
		t.Fatalf("resumed working context lost current facts/checklist or duplicated revisions: job_revision=%d checklist_revision=%d completed=%d remaining=%d summary=%t next=%t brief=%t", job.Revision, job.Checklist.Revision, job.Checklist.Completed, len(job.Checklist.Remaining), job.Progress.Summary == "Completed meaningful step π\n", job.Progress.Next == "Newer next step", job.Brief == "Retain task definition")
	}
	var history struct {
		Items   []json.RawMessage
		HasMore bool `json:"has_more"`
	}
	decode(t, freshTool("job_progress_history", map[string]any{"job_id": created.ID}), &history)
	if len(history.Items) != 2 || history.HasMore || external.Load() != 0 {
		t.Fatal("recovery duplicated progress history or contacted external service")
	}
	daemon.stop(t)
	t.Logf("uncertain workflow passed: elapsed=%s lost_committed_responses=3 daemon_restarts=3 fresh_mcp_sessions=5 changed_file_refused=true exact_handoff_recovery=true newer_progress_retained=true duplicate_effects=0 external=0 owned_processes=0", time.Since(started).Round(time.Millisecond))
}
