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
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resume real clients against accumulated synthetic history, then restart the
// service and expire replay eligibility. No live profile or provider is used.
func TestPackagedResumeAccumulatedUsage(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	private := privateFixtureDirectory(t)
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal("private resume fixture unavailable")
	}
	binDir, daemonBinary := os.Getenv("FC_SMOKE_BIN_DIR"), os.Getenv("FC_RESUME_DAEMON")
	if binDir == "" {
		binDir, _ = filepath.Abs("../../bin")
	}
	if !filepath.IsAbs(binDir) || daemonBinary != "" && !filepath.IsAbs(daemonBinary) {
		t.Fatal("absolute frozen client directory and FC_RESUME_DAEMON required")
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	if daemonBinary == "" {
		daemonBinary = filepath.Join(binDir, "fcd"+suffix)
	}
	fc, fcmcp := filepath.Join(binDir, "fc"+suffix), filepath.Join(binDir, "fcmcp"+suffix)
	hashes := map[string][32]byte{fc: binaryHash(t, fc), fcmcp: binaryHash(t, fcmcp), daemonBinary: binaryHash(t, daemonBinary)}
	t.Cleanup(func() {
		for name, hash := range hashes {
			if binaryHash(t, name) != hash {
				t.Error("frozen executable changed")
			}
		}
	})
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	t.Cleanup(deny.Close)
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	for _, name := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	data := filepath.Join(private, "data")
	bootstrap := startDaemon(t, ctx, daemonBinary, data, env)
	token, err := os.ReadFile(filepath.Join(data, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal("fixture bootstrap unavailable")
	}
	bootstrap.stop(t)
	project, worker, other, credential := seedAccess(t, ctx, data, bootstrap.instance, strings.TrimSpace(string(token)), private)
	daemon := startDaemon(t, ctx, daemonBinary, data, env)
	var targetMu sync.RWMutex
	target, _ := url.Parse("http://" + daemon.address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		targetMu.RLock()
		defer targetMu.RUnlock()
		originalDirector(r)
		r.URL.Host = target.Host
	}
	gateway := httptest.NewServer(proxy)
	t.Cleanup(func() { gateway.CloseClientConnections(); gateway.Close() })
	binding := profile.Binding{Context: profile.Context{Profile: "resume", ServerURL: gateway.URL, InstanceID: daemon.instance, ProjectID: project, WorkerID: worker}, CredentialFile: credential}
	service, err := client.New(ctx, binding)
	if err != nil {
		t.Fatal("synthetic worker client unavailable")
	}
	t.Cleanup(service.Close)
	otherBinding := binding
	otherBinding.Context.WorkerID = other
	sender, err := client.New(ctx, otherBinding)
	if err != nil {
		t.Fatal("synthetic sender unavailable")
	}
	t.Cleanup(sender.Close)
	key := 0
	nextKey := func() string { key++; return fmt.Sprintf("resume-%d", key) }
	createJob := func(title, status, assigned, brief string) client.JobReceipt {
		t.Helper()
		r, e := service.CreateJob(ctx, nextKey(), client.CreateJobRequest{Title: title, Status: status, WorkerID: assigned, Brief: brief})
		if e != nil {
			t.Fatal("synthetic job create failed")
		}
		return r
	}
	for i := 0; i < 25; i++ {
		createJob(fmt.Sprintf("Historical completed job %02d", i), "completed", worker, "Historical work")
	}
	brief := "# Current scope\n\n" + strings.Repeat("Preserve this exact working context π. ", 700)
	current := createJob("Resume this work", "active", worker, brief)
	createJob("Blocked work", "blocked", worker, "Waiting for input")
	createJob("Parked work", "parked", worker, "Deferred work")
	createJob("Other worker work", "active", other, "Outside assigned work")
	progress, next := strings.Repeat("Current position π. ", 150), "Read the remaining plan and relevant pending reminders"
	if _, err = service.UpdateJob(ctx, nextKey(), current.ID, client.UpdateJobRequest{ExpectedRevision: 1, Progress: &client.ProgressPatch{Summary: &progress, Next: &next}}); err != nil {
		t.Fatal("synthetic progress failed")
	}
	items := []client.ChecklistItem{{ID: "finished", Text: "Already done", Done: true}, {ID: "next", Text: "Continue next"}, {ID: "later", Text: "Later step"}}
	if _, err = service.ReplaceJobChecklist(ctx, nextKey(), current.ID, client.ReplaceChecklistRequest{ExpectedJobRevision: 2, ExpectedRevision: 0, Items: items}); err != nil {
		t.Fatal("synthetic plan failed")
	}
	if _, err = service.ReplacePrimaryJob(ctx, nextKey(), client.ReplacePrimaryJobRequest{ExpectedRevision: 0, JobID: current.ID}); err != nil {
		t.Fatal("synthetic focus failed")
	}
	createNote := func(job, assigned, body string) client.NoteReceipt {
		t.Helper()
		r, e := service.CreateNote(ctx, nextKey(), client.CreateNoteRequest{JobID: job, WorkerID: assigned, Kind: "reminder", Body: body})
		if e != nil {
			t.Fatal("synthetic note failed")
		}
		return r
	}
	consumed := "consumed"
	for i := 0; i < 21; i++ {
		n := createNote("", worker, "Resolved historical reminder")
		if _, err = service.UpdateNote(ctx, nextKey(), n.ID, client.UpdateNoteRequest{ExpectedRevision: 1, Status: &consumed}); err != nil {
			t.Fatal("synthetic note resolution failed")
		}
	}
	jobNoteIDs := map[string]bool{}
	for i := 0; i < 7; i++ {
		n := createNote(current.ID, "", fmt.Sprintf("Pending exact-job reminder %d", i))
		jobNoteIDs[n.ID] = true
	}
	workerBody := "# Worker-wide instruction\n\n" + strings.Repeat("Use the selected project context π. ", 350)
	workerNote := createNote("", worker, workerBody)
	createNote("", other, "Other worker reminder must stay out")
	for i := 0; i < 25; i++ {
		r, e := sender.SendMessage(ctx, nextKey(), client.SendMessageRequest{RecipientWorkerID: worker, Body: "Handled conversation"})
		if e != nil {
			t.Fatal("synthetic message failed")
		}
		if _, err = service.Acknowledge(ctx, nextKey(), r.ID); err != nil {
			t.Fatal("synthetic acknowledgement failed")
		}
	}
	before, err := service.InboxSnapshot(ctx, client.PageOptions{}, client.InboxFilter{Unacknowledged: true})
	if err != nil || len(before.Inbox.Items) != 0 {
		t.Fatal("handled history remained pending")
	}
	body := "  # New full instruction π\n" + strings.Repeat("Do the next selected step. ", 450) + "\n  "
	pending := []client.MessageReceipt{}
	for i := 0; i < 2; i++ {
		r, e := sender.SendMessage(ctx, nextKey(), client.SendMessageRequest{RecipientWorkerID: worker, Body: body})
		if e != nil {
			t.Fatal("pending message failed")
		}
		pending = append(pending, r)
	}
	if _, err = service.MarkSeen(ctx, nextKey(), pending[0].ID); err != nil {
		t.Fatal("seen pending fixture failed")
	}
	profiles := filepath.Join(private, "resume-profiles.json")
	base := []string{"--profiles-file", profiles, "--profile", "resume"}
	call := func(args ...string) json.RawMessage {
		t.Helper()
		return runCLI(t, ctx, fc, env, append(append([]string{}, base...), args...)...)
	}
	call("profile", "create", "--server", gateway.URL, "--instance", daemon.instance, "--project", project, "--worker", worker, "--credential-file", credential)
	var primary client.PrimaryJob
	var assigned client.Page[client.JobSummary]
	var job client.Job
	var notes client.Page[client.NoteSummary]
	var inbox client.Page[client.InboxItem]
	verifyJob := func() {
		t.Helper()
		if job.ID != current.ID || job.Brief != brief || job.Progress.Summary != progress || job.Progress.Next != next || job.Checklist.Completed != 1 || !reflect.DeepEqual(job.Checklist.Remaining, items[1:]) || len(job.Notes.Items) != 5 || !job.Notes.HasMore || job.Notes.Cursor == "" {
			t.Fatal("full current working context was incomplete without explicit continuation")
		}
		for _, n := range job.Notes.Items {
			if !jobNoteIDs[n.ID] {
				t.Fatal("job read mixed worker/other notes")
			}
		}
	}
	verifyNotes := func() {
		t.Helper()
		if len(notes.Items) != 8 || notes.HasMore {
			t.Fatal("worker notes did not select pending direct and assigned-job reminders")
		}
		found := false
		for _, n := range notes.Items {
			if n.ID == workerNote.ID {
				found = true
				if !n.BodyTruncated || n.BodyPreview == workerBody {
					t.Fatal("large worker reminder preview was misleading")
				}
			} else if !jobNoteIDs[n.ID] {
				t.Fatal("worker notes leaked unrelated or handled reminders")
			}
		}
		if !found {
			t.Fatal("worker-only reminder absent")
		}
	}
	verifyInbox := func() {
		t.Helper()
		if len(inbox.Items) != 2 || inbox.HasMore || !inbox.Items[0].Seen || inbox.Items[0].Acknowledged || inbox.Items[1].Acknowledged {
			t.Fatal("pending inbox lost seen-unhandled work or included handled history")
		}
	}
	call("doctor")
	decode(t, call("jobs", "primary"), &primary)
	if primary.JobID != current.ID {
		t.Fatal("resume lost selected focus")
	}
	decode(t, call("jobs", "assigned"), &assigned)
	if len(assigned.Items) != 3 || assigned.HasMore {
		t.Fatal("default assigned jobs included completed history or other worker")
	}
	var allAssigned client.Page[client.JobSummary]
	decode(t, call("jobs", "assigned", "--all", "--limit", "100"), &allAssigned)
	if len(allAssigned.Items) != 28 || allAssigned.HasMore {
		t.Fatal("explicit assignment history lost completed work")
	}
	decode(t, call("jobs", "read", primary.JobID), &job)
	verifyJob()
	// Job reads intentionally omit worker-only notes: resume needs this scoped read.
	decode(t, call("notes", "list"), &notes)
	verifyNotes()
	var fullNote client.Note
	decode(t, call("notes", "read", workerNote.ID), &fullNote)
	if fullNote.Body != workerBody {
		t.Fatal("selected worker reminder lost full body")
	}
	decode(t, call("inbox", "list"), &inbox)
	verifyInbox()
	var fullMessage client.Message
	decode(t, call("inbox", "read", pending[0].ID), &fullMessage)
	if fullMessage.Body != body {
		t.Fatal("selected pending message lost full body")
	}
	// Follow the default job-note cursor through its owning list and retain filters.
	var remaining client.Page[client.NoteSummary]
	decode(t, call("notes", "list", "--all-workers", "--job", current.ID, "--limit", "5", "--cursor", job.Notes.Cursor), &remaining)
	if len(remaining.Items) != 2 || remaining.HasMore {
		t.Fatal("job-note continuation was unusable through bound CLI")
	}
	for _, n := range remaining.Items {
		if !jobNoteIDs[n.ID] {
			t.Fatal("continuation returned unrelated note")
		}
	}

	command := exec.CommandContext(ctx, fcmcp, base...)
	command.Env = env
	diagnostics := &boundedOutput{}
	command.Stderr = diagnostics
	bounded, stop := context.WithTimeout(ctx, stepTimeout)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "resume-usage", Version: "1"}, nil).Connect(bounded, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	stop()
	if err != nil {
		if command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		t.Fatal("actual SDK MCP resume connection failed")
	}
	var closeOnce sync.Once
	closeMCP := func() {
		closeOnce.Do(func() {
			done := make(chan error, 1)
			go func() { done <- session.Close() }()
			select {
			case e := <-done:
				if e != nil || diagnostics.len() != 0 {
					t.Error("owned MCP did not close cleanly")
				}
			case <-time.After(stepTimeout):
				_ = command.Process.Kill()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("owned MCP did not join after kill")
				}
				t.Error("owned MCP close timed out")
			}
		})
	}
	t.Cleanup(closeMCP)
	tool := func(name string, args map[string]any) json.RawMessage {
		t.Helper()
		if args == nil {
			args = map[string]any{}
		}
		c, s := context.WithTimeout(ctx, stepTimeout)
		defer s()
		r, e := session.CallTool(c, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil || r == nil || r.IsError {
			var failure struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if r != nil {
				data, _ := json.Marshal(r.StructuredContent)
				_ = json.Unmarshal(data, &failure)
			}
			t.Fatalf("MCP resume operation %s failed: transport=%v code=%s message=%s", name, e, failure.Error.Code, failure.Error.Message)
		}
		out, e := json.Marshal(r.StructuredContent)
		if e != nil {
			t.Fatal("MCP resume content failed")
		}
		return out
	}
	decode(t, tool("job_primary_read", nil), &primary)
	decode(t, tool("job_read", map[string]any{"job_id": primary.JobID}), &job)
	verifyJob()
	decode(t, tool("note_list", nil), &notes)
	verifyNotes()
	decode(t, tool("note_list", map[string]any{"all_workers": true, "job_id": current.ID, "limit": 5, "cursor": job.Notes.Cursor}), &remaining)
	if len(remaining.Items) != 2 || remaining.HasMore {
		t.Fatal("job note continuation was unusable through MCP")
	}
	decode(t, tool("jobs_assigned", nil), &assigned)
	if len(assigned.Items) != 3 || assigned.HasMore {
		t.Fatal("MCP default assignments lost current owned work")
	}
	decode(t, tool("jobs_assigned", map[string]any{"status": "all", "limit": 100}), &allAssigned)
	if len(allAssigned.Items) != 28 || allAssigned.HasMore {
		t.Fatal("MCP explicit assignment history lost completed work")
	}
	decode(t, tool("inbox_list", nil), &inbox)
	verifyInbox()
	decode(t, tool("note_read", map[string]any{"note_id": workerNote.ID}), &fullNote)
	if fullNote.Body != workerBody {
		t.Fatal("MCP lost full worker reminder")
	}
	decode(t, tool("inbox_message_read", map[string]any{"message_id": pending[0].ID}), &fullMessage)
	if fullMessage.Body != body {
		t.Fatal("MCP lost full pending message")
	}

	restart := func() {
		t.Helper()
		daemon = startDaemon(t, ctx, daemonBinary, data, env)
		if daemon.instance != bootstrap.instance {
			t.Fatal("restart changed instance")
		}
		newTarget, _ := url.Parse("http://" + daemon.address)
		targetMu.Lock()
		target = newTarget
		targetMu.Unlock()
	}
	daemon.stop(t)
	restart()
	// Same MCP process and profile survive a daemon restart via stable address.
	decode(t, tool("job_read", map[string]any{"job_id": current.ID}), &job)
	verifyJob()
	decode(t, tool("note_list", nil), &notes)
	verifyNotes()
	decode(t, call("inbox", "list"), &inbox)
	verifyInbox()
	var events client.MessageEventPage
	decode(t, call("inbox", "events", "--cursor", before.EventCursor), &events)
	sent := 0
	for _, e := range events.Items {
		if e.Type == "message.sent" {
			sent++
		}
	}
	if sent != 2 {
		t.Fatal("restart replay lost the pending messages")
	}

	// Fixture-only time aging: stop process and move event retention timestamps.
	// Messages, receipts and note/job state are untouched; this is not production API.
	daemon.stop(t)
	store, err := core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped fixture unavailable for replay aging")
	}
	_, err = store.DB().ExecContext(ctx, "UPDATE events SET created_at=?", time.Now().UTC().Add(-8*24*time.Hour).Format(time.RFC3339Nano))
	closeErr := store.Close()
	if err != nil || closeErr != nil {
		t.Fatal("fixture retention aging failed")
	}
	restart()
	bounded, stop = context.WithTimeout(ctx, stepTimeout)
	expired, err := session.CallTool(bounded, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"cursor": before.EventCursor, "wait_seconds": 1}})
	stop()
	if err != nil || expired == nil || !expired.IsError {
		t.Fatal("expired cursor did not report resync")
	}
	failure, _ := json.Marshal(expired.StructuredContent)
	if !strings.Contains(string(failure), "resync_required") || !strings.Contains(string(failure), "snapshot") {
		t.Fatal("resync response did not give useful recovery guidance")
	}
	var fresh client.InboxSnapshot
	decode(t, tool("inbox_snapshot", map[string]any{"limit": 1}), &fresh)
	if len(fresh.Inbox.Items) != 1 || !fresh.Inbox.HasMore || fresh.EventCursor == "" {
		t.Fatal("fresh snapshot lost pending page or replay watermark")
	}
	decode(t, tool("inbox_list", map[string]any{"limit": 1, "cursor": fresh.Inbox.Cursor}), &inbox)
	if len(inbox.Items) != 1 || inbox.Items[0].ID != pending[1].ID || inbox.HasMore || inbox.Items[0].Acknowledged {
		t.Fatal("resync pending continuation failed")
	}
	final, err := sender.SendMessage(ctx, nextKey(), client.SendMessageRequest{RecipientWorkerID: worker, Body: "Post-resync new message"})
	if err != nil {
		t.Fatal("post-resync send failed")
	}
	decode(t, tool("wait_for_messages", map[string]any{"cursor": fresh.EventCursor, "wait_seconds": 1}), &events)
	if len(events.Items) != 1 || events.Items[0].ResourceID != final.ID {
		t.Fatal("snapshot-resubscribe lost later incoming message")
	}
	decode(t, tool("job_read", map[string]any{"job_id": current.ID}), &job)
	verifyJob()
	// Reassignment clears the old focus and updates scoped note selection atomically.
	if _, err = service.UpdateJob(ctx, nextKey(), current.ID, client.UpdateJobRequest{ExpectedRevision: 2, WorkerID: &other}); err != nil {
		t.Fatal("synthetic reassignment failed")
	}
	decode(t, tool("job_primary_read", nil), &primary)
	decode(t, tool("note_list", nil), &notes)
	decode(t, call("jobs", "assigned"), &assigned)
	if primary.JobID != "" || primary.Revision != 2 || len(notes.Items) != 1 || notes.Items[0].ID != workerNote.ID || len(assigned.Items) != 2 {
		t.Fatal("reassignment retained stale focus, job notes or assignment")
	}
	if _, err = service.UpdateJob(ctx, nextKey(), current.ID, client.UpdateJobRequest{ExpectedRevision: 3, WorkerID: &worker}); err != nil {
		t.Fatal("synthetic return assignment failed")
	}
	decode(t, call("jobs", "primary"), &primary)
	decode(t, call("jobs", "assigned"), &assigned)
	decode(t, tool("note_list", nil), &notes)
	verifyNotes()
	if primary.JobID != "" || len(assigned.Items) != 3 {
		t.Fatal("missing focus incorrectly suppressed assigned resume work")
	}
	decode(t, call("inbox", "list"), &inbox)
	if len(inbox.Items) != 3 {
		t.Fatal("resume or resync implicitly handled pending messages")
	}
	for _, message := range inbox.Items {
		if message.Acknowledged {
			t.Fatal("resume implicitly acknowledged a message")
		}
	}
	closeMCP()
	if external.Load() != 0 {
		t.Fatal("unexpected external work")
	}
	t.Logf("accumulated resume passed elapsed=%s jobs=29 completed=25 open_assigned=3 notes=30 pending_relevant=8 handled_messages=25 pending_messages=2 full_brief_bytes=%d worker_note_bytes=%d message_bytes=%d CLI_and_official_SDK=true restart_same_MCP=true expired_cursor_snapshot_continuation=true no_implicit_handling=true external=0", time.Since(started).Round(time.Millisecond), len(brief), len(workerBody), len(body))
}
