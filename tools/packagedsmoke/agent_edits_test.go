//go:build packaged_smoke

package packagedsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// Exercise the actual frozen executables against synthetic private local state.
// This is a bounded usability contract check, not a load or provider test.
func TestPackagedAgentEditsSmoke(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private agent edit fixture unavailable")
	}
	binDir := os.Getenv("FC_SMOKE_BIN_DIR")
	if !filepath.IsAbs(binDir) {
		t.Fatal("FC_SMOKE_BIN_DIR must select absolute frozen executables")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	fc, fcd, fcmcp := filepath.Join(binDir, "fc"+suffix), filepath.Join(binDir, "fcd"+suffix), filepath.Join(binDir, "fcmcp"+suffix)
	hashes := map[string][32]byte{fc: binaryHash(t, fc), fcd: binaryHash(t, fcd), fcmcp: binaryHash(t, fcmcp)}
	t.Cleanup(func() {
		for file, hash := range hashes {
			if binaryHash(t, file) != hash {
				t.Error("frozen executable changed")
			}
		}
	})
	var external, messageGets, threadGets, itemPatches, itemBytes atomic.Int64
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
	token, err := os.ReadFile(adminFile)
	if err != nil {
		t.Fatal("fixture bootstrap credential unavailable")
	}
	bootstrap.stop(t)
	project, worker, recipient, _ := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(token)), private)
	daemon := startDaemon(t, ctx, fcd, data, env)
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/messages/") {
			messageGets.Add(1)
		}
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/conversations/") {
			threadGets.Add(1)
		}
		if r.Method == "PATCH" && strings.Contains(r.URL.Path, "/checklist/items/") {
			body, readErr := io.ReadAll(io.LimitReader(r.Body, 257))
			r.Body.Close()
			if readErr != nil || len(body) > 256 || bytes.Contains(body, []byte("Preserved completed")) {
				w.WriteHeader(413)
				return
			}
			itemPatches.Add(1)
			itemBytes.Add(int64(len(body)))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	base := []string{"--profiles-file", filepath.Join(private, "profiles.json"), "--profile", "builder"}
	call := func(args ...string) json.RawMessage {
		t.Helper()
		body := runCLI(t, ctx, fc, env, append(append([]string{}, base...), args...)...)
		if bytes.Contains(body, bytes.TrimSpace(token)) {
			t.Fatal("credential disclosed")
		}
		return body
	}
	refuse := func(args ...string) {
		t.Helper()
		bounded, stop := context.WithTimeout(ctx, stepTimeout)
		defer stop()
		command := exec.CommandContext(bounded, fc, append(append([]string{}, base...), args...)...)
		command.Env, command.WaitDelay = env, time.Second
		out, diagnostic := &boundedOutput{}, &boundedOutput{}
		command.Stdout, command.Stderr = out, diagnostic
		if command.Run() == nil || out.len() != 0 || diagnostic.len() > 4096 || !json.Valid(diagnostic.bytes()) || bytes.Contains(diagnostic.bytes(), bytes.TrimSpace(token)) {
			t.Fatal("guarded item refusal failed")
		}
	}
	file := func(name, text string) string {
		t.Helper()
		path := filepath.Join(private, name)
		if os.WriteFile(path, []byte(text), 0600) != nil {
			t.Fatal("private text fixture unavailable")
		}
		return path
	}
	call("profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", adminFile)
	var last, previous client.MessageReceipt
	body := "  ## Exact π reply\n\n" + strings.Repeat("λ", 4000) + "\n--help\n  "
	for i := 0; i < 25; i++ {
		previous = last
		args := []string{"inbox", "send", "--to", recipient, "--key", fmt.Sprintf("agent-message-%d", i)}
		if i == 24 {
			args = append(args, "--body-file", file("message.md", body))
		} else {
			args = append(args, "--body", "Older conversation text")
		}
		if previous.ID != "" {
			args = append(args, "--reply-to", previous.ID)
		}
		decode(t, call(args...), &last)
	}
	var replay client.MessageReceipt
	decode(t, call("inbox", "send", "--to", recipient, "--body", body, "--reply-to", previous.ID, "--key", "agent-message-24"), &replay)
	if replay != last {
		t.Fatal("file/inline message replay changed identity")
	}
	var selected client.Message
	decode(t, call("inbox", "read", last.ID), &selected)
	if selected.ID != last.ID || selected.Body != body || selected.ReplyToMessageID != previous.ID || messageGets.Load() != 1 || threadGets.Load() != 0 {
		t.Fatal("exact selected reply loaded history or changed bytes")
	}
	brief, progress, next, blocker := "  # Brief π\n\nExact text\t\n", "  Working π\n", "\nNext step\t\n", "  Waiting\n"
	created := call("jobs", "create", "--title", "Agent edit fixture", "--brief-file", file("brief.md", brief), "--worker", worker, "--key", "agent-job")
	if !bytes.Equal(created, call("jobs", "create", "--title", "Agent edit fixture", "--brief", brief, "--worker", worker, "--key", "agent-job")) {
		t.Fatal("file/inline job create replay changed")
	}
	var job client.Job
	var receipt struct {
		ID string `json:"id"`
	}
	decode(t, created, &receipt)
	jobID := receipt.ID
	updatedBrief := brief + "\nMore context\n"
	update := call("jobs", "update", jobID, "--expect-revision", "1", "--brief-file", file("updated-brief.md", updatedBrief), "--progress-file", file("progress.md", progress), "--next-file", file("next.md", next), "--blocker-file", file("blocker.md", blocker), "--key", "agent-progress")
	if !bytes.Equal(update, call("jobs", "update", jobID, "--expect-revision", "1", "--brief", updatedBrief, "--progress", progress, "--next", next, "--blocker", blocker, "--key", "agent-progress")) {
		t.Fatal("file/inline progress replay changed")
	}
	items := []client.ChecklistItem{{ID: "done", Text: "Preserved completed text π", Done: true}, {ID: "next", Text: "Selected remaining item"}, {ID: "later", Text: "Untouched remaining item"}}
	encoded, _ := json.Marshal(items)
	call("jobs", "checklist-set", jobID, "--expect-revision", "2", "--expect-checklist-revision", "0", "--items-file", file("checklist.json", string(encoded)), "--key", "agent-plan")
	decode(t, call("jobs", "read", jobID), &job)
	if job.Brief != updatedBrief || job.Progress.Summary != progress || job.Progress.Next != next || job.Progress.Blocker != blocker || job.Checklist.Revision != 1 || job.Checklist.Completed != 1 || !reflect.DeepEqual(job.Checklist.Remaining, items[1:]) {
		t.Fatal("default job read lost exact text or remaining plan")
	}
	// A newer unrelated progress edit must not invalidate the checklist guard.
	call("jobs", "update", jobID, "--expect-revision", strconv.FormatInt(job.Revision, 10), "--progress", "Newer unrelated progress", "--key", "agent-newer-progress")
	args := []string{"jobs", "checklist-item", jobID, "next", "--done", "true", "--expect-checklist-revision", "1", "--key", "agent-tick"}
	tick := call(args...)
	if !bytes.Equal(tick, call(args...)) || len(tick) > 512 {
		t.Fatal("tiny checklist item replay failed")
	}
	var noOp client.ChecklistReceipt
	decode(t, call("jobs", "checklist-item", jobID, "next", "--done", "true", "--expect-checklist-revision", "2", "--key", "agent-noop"), &noOp)
	if noOp.Outcome != "unchanged" || noOp.Revision != 2 {
		t.Fatal("satisfied item change made another revision")
	}
	refuse("jobs", "checklist-item", jobID, "next", "--done", "false", "--expect-checklist-revision", "1", "--key", "agent-stale")

	command := exec.CommandContext(ctx, fcmcp, base...)
	command.Env = env
	diagnostic := &boundedOutput{}
	command.Stderr = diagnostic
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agent-edits-smoke", Version: "1"}, nil).Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stop()
	if err != nil {
		if command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Fatal("actual MCP initialization failed")
	}
	var once sync.Once
	closed := make(chan struct{})
	var closeErr error
	closeMCP := func() {
		once.Do(func() { go func() { closeErr = session.Close(); close(closed) }() })
		select {
		case <-closed:
			if closeErr != nil || diagnostic.len() != 0 || command.ProcessState == nil || !command.ProcessState.Success() {
				t.Error("actual MCP did not join cleanly")
			}
		case <-time.After(stepTimeout):
			_ = command.Process.Kill()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Error("owned MCP did not join after kill")
			}
			t.Error("MCP close exceeded bound")
		}
	}
	t.Cleanup(closeMCP)
	bounded, stop = context.WithTimeout(ctx, stepTimeout)
	result, err := session.CallTool(bounded, &mcp.CallToolParams{Name: "job_checklist_item_update", Arguments: map[string]any{"job_id": jobID, "item_id": "next", "expected_revision": 2, "done": false, "idempotency_key": "agent-mcp-reopen"}})
	stop()
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatal("actual MCP item edit failed")
	}
	mcpBody, marshalErr := json.Marshal(result.StructuredContent)
	text, ok := result.Content[0].(*mcp.TextContent)
	if marshalErr != nil || !ok || !jsonEqual(mcpBody, []byte(text.Text)) || len(mcpBody) > 512 {
		t.Fatal("MCP item receipt is not compact matching JSON")
	}
	closeMCP()
	decode(t, call("jobs", "read", jobID), &job)
	var checklist client.Checklist
	decode(t, call("jobs", "checklist", jobID), &checklist)
	if job.Progress.Summary != "Newer unrelated progress" || job.Brief != updatedBrief || job.Progress.Next != next || job.Progress.Blocker != blocker || job.Checklist.Revision != 3 || checklist.Revision != 3 || !reflect.DeepEqual(checklist.Items, items) {
		t.Fatal("item edit lost completed text, order or unrelated newer progress")
	}
	if external.Load() != 0 || itemPatches.Load() != 5 || threadGets.Load() != 0 {
		t.Fatal("unexpected external work, thread reads or item requests")
	}
	t.Logf("packaged agent edits passed: elapsed=%s thread_messages=25 exact_message_gets=%d conversation_gets=%d file_inline_replay=true item_patches=%d item_patch_bytes=%d stale_refused=true noop_revision_retained=true newer_progress_preserved=true official_sdk_mcp=true external=0 owned_processes=0", time.Since(started).Round(time.Millisecond), messageGets.Load(), threadGets.Load(), itemPatches.Load(), itemBytes.Load())
}
