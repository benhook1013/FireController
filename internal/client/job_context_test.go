package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestJobContextClientOneSnapshotAndExplicitContinuation(t *testing.T) {
	f := newMutationFixture(t)
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instance" {
			calls.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/context") && (r.URL.Query().Get("limit") != "1" && r.URL.Query().Get("limit") != "20" || len(r.URL.Query()) != 1 || r.Header.Get("X-FC-Worker") != f.sender) {
			t.Error("context selection/binding", r.URL)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	job, err := c.CreateJob(t.Context(), "context-job", client.CreateJobRequest{Title: "Selected other worker job", Brief: "Original brief", WorkerID: f.recipient})
	if err != nil {
		t.Fatal(err)
	}
	before := f.eventCount(t)
	start := calls.Load()
	empty, err := c.JobContext(t.Context(), job.ID, 0)
	if err != nil || !empty.Complete || empty.Omitted == nil || len(empty.Omitted) != 0 || empty.Checkpoint != nil || empty.WorkerID != f.sender || empty.Job.WorkerID != f.recipient || empty.Checklist.Items == nil || empty.Primary.Revision != 0 || empty.Pause.Revision != 0 || calls.Load() != start+1 || f.eventCount(t) != before {
		t.Fatal("empty combined read", empty, err, calls.Load()-start)
	}
	checkpoint, err := c.AppendCheckpoint(t.Context(), "context-checkpoint", job.ID, client.AppendCheckpointRequest{ExpectedJobRevision: 1, Done: "Retained captured progress", Next: "Continue later", Evidence: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	brief := strings.Repeat("<", 32<<10)
	if _, err = c.EditJob(t.Context(), "context-brief", job.ID, client.EditJobRequest{ExpectedRevision: 1, Brief: &brief}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReplaceJobChecklist(t.Context(), "context-checklist", job.ID, client.ReplaceChecklistRequest{ExpectedJobRevision: 2, Items: []client.ChecklistItem{{ID: "step", Text: "Current checklist"}}}); err != nil {
		t.Fatal(err)
	}
	own, err := c.CreateJob(t.Context(), "context-owned-job", client.CreateJobRequest{Title: "Caller assignment", Brief: "Own", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReplacePrimaryJob(t.Context(), "context-primary", client.ReplacePrimaryJobRequest{JobID: own.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReplaceWorkerPause(t.Context(), "context-pause", client.ReplaceWorkerPauseRequest{Paused: true, Reason: "Coordination pause"}); err != nil {
		t.Fatal(err)
	}
	for i, in := range []client.CreateNoteRequest{{WorkerID: f.sender, Kind: "reminder", Body: "short pending note"}, {WorkerID: f.recipient, JobID: own.ID, Kind: "source", Body: strings.Repeat("界", 100)}, {Phase: "Unselected phase", Kind: "reminder", Body: "not caller context"}} {
		if _, err = c.CreateNote(t.Context(), "context-note-"+string(rune('a'+i)), in); err != nil {
			t.Fatal(err)
		}
	}
	other := f.newClient(t, f.admin, f.recipient, service.URL)
	first, err := other.SendMessage(t.Context(), "context-message1", client.SendMessageRequest{RecipientWorkerID: f.sender, Body: "Seen but still unhandled"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.MarkSeen(t.Context(), "context-seen", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = other.SendMessage(t.Context(), "context-message2", client.SendMessageRequest{RecipientWorkerID: f.sender, Body: "second pending"}); err != nil {
		t.Fatal(err)
	}
	before = f.eventCount(t)
	start = calls.Load()
	got, err := c.JobContext(t.Context(), job.ID, 1)
	if err != nil || got.Complete || !reflect.DeepEqual(got.Omitted, []string{"note_bodies", "additional_notes", "message_bodies", "additional_messages"}) || got.Job.Brief != brief || got.Job.Revision != 2 || got.Checkpoint == nil || got.Checkpoint.ID != checkpoint.ID || got.Checkpoint.JobRevision != 1 || got.Checklist.JobRevision != 2 || got.Checklist.Revision != 1 || got.Primary.JobID != own.ID || !got.Pause.Paused || !got.Inbox.Items[0].Seen || got.Inbox.Items[0].Acknowledged || calls.Load() != start+1 || f.eventCount(t) != before {
		t.Fatal("mixed/fanout/omission", got, err, calls.Load()-start)
	}
	notes, err := c.Notes(t.Context(), client.PageOptions{Limit: 1, Cursor: got.Notes.Cursor}, client.NoteFilter{WorkerID: f.sender, IncludeAssignedJobs: true})
	if err != nil || len(notes.Items) != 1 || notes.HasMore {
		t.Fatal("notes continuation", notes, err)
	}
	inbox, err := c.Inbox(t.Context(), client.PageOptions{Limit: 1, Cursor: got.Inbox.Cursor}, client.InboxFilter{Unacknowledged: true})
	if err != nil || len(inbox.Items) != 1 || inbox.HasMore {
		t.Fatal("inbox continuation", inbox, err)
	}
	sent, err := other.SendMessage(t.Context(), "context-after-watermark", client.SendMessageRequest{RecipientWorkerID: f.sender, Body: "after snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.MessageEvents(t.Context(), got.EventCursor, 20, 0)
	if err != nil || len(events.Items) != 1 || events.Items[0].ResourceID != sent.ID {
		t.Fatal("context event handoff", events, err)
	}
	if f.providerCalls.Load() != 1 {
		t.Fatal("context used provider")
	}
}

func emptyJobContextResponse() client.JobContextResult {
	return client.JobContextResult{ProjectID: "project-one", WorkerID: "worker-one", Job: client.Job{ID: "job-one", ProjectID: "project-one", Title: "Selected work", Brief: "Untruncated brief", WorkerID: "worker-other", Status: "active", Revision: 1, CreatedAt: "2026-10-16T00:00:00Z", UpdatedAt: "2026-10-15T00:00:00Z"}, Checklist: client.Checklist{JobID: "job-one", JobRevision: 1, Items: []client.ChecklistItem{}}, Primary: client.PrimaryJob{WorkerID: "worker-one"}, Pause: client.WorkerPause{WorkerID: "worker-one"}, Notes: client.Page[client.NoteSummary]{Items: []client.NoteSummary{}}, Inbox: client.Page[client.InboxItem]{Items: []client.InboxItem{}}, EventCursor: "opaque-context-events", Complete: true, Omitted: []string{}}
}
func TestJobContextClientStrictRequiredAndNullableFields(t *testing.T) {
	good := emptyJobContextResponse()
	raw, _ := json.Marshal(good)
	c, _ := progressResponseClient(t, string(raw))
	if _, err := c.JobContext(t.Context(), "job-one", 0); err != nil {
		t.Fatal("explicit null checkpoint rejected", err)
	}
	var full map[string]json.RawMessage
	json.Unmarshal(raw, &full)
	for field := range full {
		for _, null := range []bool{false, true} {
			if field == "checkpoint" && null {
				continue
			}
			t.Run(field+map[bool]string{true: "/null", false: "/missing"}[null], func(t *testing.T) {
				var altered map[string]json.RawMessage
				json.Unmarshal(raw, &altered)
				if null {
					altered[field] = json.RawMessage("null")
				} else {
					delete(altered, field)
				}
				body, _ := json.Marshal(altered)
				c, _ := progressResponseClient(t, string(body))
				_, err := c.JobContext(t.Context(), "job-one", 20)
				checkError(t, err, "invalid_response")
			})
		}
	}
	for _, change := range []func(*client.JobContextResult){
		func(v *client.JobContextResult) { v.ProjectID = "foreign" }, func(v *client.JobContextResult) { v.WorkerID = "worker-other" }, func(v *client.JobContextResult) { v.Job.ID = "job-other" }, func(v *client.JobContextResult) { v.Job.CreatedAt = "not-time" }, func(v *client.JobContextResult) { v.Checklist.JobRevision = 2 }, func(v *client.JobContextResult) { v.Primary.WorkerID = "other" }, func(v *client.JobContextResult) { v.Primary.JobID = "job-one" }, func(v *client.JobContextResult) { v.Pause.Paused = true }, func(v *client.JobContextResult) { v.EventCursor = "" }, func(v *client.JobContextResult) { v.Complete = false }, func(v *client.JobContextResult) { v.Omitted = []string{"note_bodies"} }, func(v *client.JobContextResult) { v.Notes.HasMore = true; v.Notes.Cursor = "next" },
	} {
		v := emptyJobContextResponse()
		change(&v)
		body, _ := json.Marshal(v)
		c, _ := progressResponseClient(t, string(body))
		_, err := c.JobContext(t.Context(), "job-one", 20)
		checkError(t, err, "invalid_response")
	}
	checkpoint := client.Checkpoint{ID: "checkpoint-one", ProjectID: "project-one", JobID: "job-one", JobRevision: 1, Sequence: 1, ActorID: "actor-one", Done: "Retained", Next: "Continue", Evidence: []string{}, CreatedAt: "2026-10-16T00:00:00Z"}
	withCheckpoint := emptyJobContextResponse()
	withCheckpoint.Checkpoint = &checkpoint
	valid, _ := json.Marshal(withCheckpoint)
	c, _ = progressResponseClient(t, string(valid))
	if _, err := c.JobContext(t.Context(), "job-one", 20); err != nil {
		t.Fatal("valid checkpoint", err)
	}
	for _, mutate := range []func(*client.Checkpoint){func(cp *client.Checkpoint) { cp.JobRevision = 2 }, func(cp *client.Checkpoint) { cp.JobID = "other-job" }, func(cp *client.Checkpoint) { cp.CreatedAt = "0001-01-01T00:00:00Z" }} {
		altered := checkpoint
		mutate(&altered)
		in := emptyJobContextResponse()
		in.Checkpoint = &altered
		body, _ := json.Marshal(in)
		c, _ := progressResponseClient(t, string(body))
		_, err := c.JobContext(t.Context(), "job-one", 20)
		checkError(t, err, "invalid_response")
	}
	for _, body := range []string{strings.Replace(string(raw), `"complete":true`, `"complete":true,"complete":false`, 1), strings.Replace(string(raw), `"paused":false`, `"paused":null`, 1), strings.Replace(string(raw), `"has_more":false`, `"has_more":null`, 1), strings.Replace(string(raw), `"revision":1`, `"revision":1.0`, 1), strings.Replace(string(raw), `"checkpoint":null`, `"checkpoint":{}`, 1), strings.TrimSuffix(string(raw), "}") + `,"unexpected":true}`} {
		c, _ := progressResponseClient(t, body)
		_, err := c.JobContext(t.Context(), "job-one", 20)
		checkError(t, err, "invalid_response")
	}
}
func TestJobContextClientPendingPreviewDisclosureAndByteBound(t *testing.T) {
	v := emptyJobContextResponse()
	v.Notes.Items = []client.NoteSummary{{ID: "note-one", ProjectID: v.ProjectID, WorkerID: v.WorkerID, Kind: "reminder", Status: "pending", Revision: 1, CreatedAt: v.Job.CreatedAt, UpdatedAt: v.Job.CreatedAt, BodyPreview: "short whole preview"}}
	v.Inbox.Items = []client.InboxItem{{ID: "message-one", ConversationID: "conversation-one", SenderWorkerID: "sender-one", RecipientWorkerID: v.WorkerID, Preview: "short whole preview", Seen: true, CreatedAt: v.Job.CreatedAt}}
	v.Complete = false
	v.Omitted = []string{"note_bodies", "message_bodies"}
	raw, _ := json.Marshal(v)
	c, _ := progressResponseClient(t, string(raw))
	if _, err := c.JobContext(t.Context(), "job-one", 1); err != nil {
		t.Fatal("short previews not disclosed", err)
	}
	for _, change := range []func(*client.JobContextResult){func(v *client.JobContextResult) { v.Omitted = []string{"message_bodies", "note_bodies"} }, func(v *client.JobContextResult) { v.Complete = true }, func(v *client.JobContextResult) { v.Notes.Items[0].Status = "consumed" }, func(v *client.JobContextResult) { v.Notes.Items[0].BodyTruncated = true }, func(v *client.JobContextResult) { v.Inbox.Items[0].Acknowledged = true }, func(v *client.JobContextResult) { v.Inbox.Items[0].RecipientWorkerID = "other" }, func(v *client.JobContextResult) { v.Inbox.Items[0].PreviewTruncated = true }} {
		copy := v
		copy.Notes.Items = append([]client.NoteSummary{}, v.Notes.Items...)
		copy.Inbox.Items = append([]client.InboxItem{}, v.Inbox.Items...)
		change(&copy)
		body, _ := json.Marshal(copy)
		c, _ := progressResponseClient(t, string(body))
		_, err := c.JobContext(t.Context(), "job-one", 1)
		checkError(t, err, "invalid_response")
	}
	for _, body := range []string{
		strings.Replace(string(raw), `,"body_truncated":false`, "", 1),
		strings.Replace(string(raw), `"body_truncated":false`, `"body_truncated":null`, 1),
		strings.Replace(string(raw), `"preview_truncated":false,`, "", 1),
		strings.Replace(string(raw), `"preview_truncated":false`, `"preview_truncated":null`, 1),
	} {
		if body == string(raw) {
			t.Fatal("negative fixture did not alter the response")
		}
		c, _ := progressResponseClient(t, body)
		_, err := c.JobContext(t.Context(), "job-one", 1)
		checkError(t, err, "invalid_response")
	}
	checklist := emptyJobContextResponse()
	checklist.Checklist.Revision = 1
	checklist.Checklist.Items = []client.ChecklistItem{{ID: "step", Text: "Explicit false"}}
	checked, _ := json.Marshal(checklist)
	for _, body := range []string{strings.Replace(string(checked), `,"done":false`, "", 1), strings.Replace(string(checked), `"done":false`, `"done":null`, 1)} {
		c, _ := progressResponseClient(t, body)
		_, err := c.JobContext(t.Context(), "job-one", 1)
		checkError(t, err, "invalid_response")
	}
	empty, _ := json.Marshal(emptyJobContextResponse())
	c, _ = progressResponseClient(t, string(empty)+strings.Repeat(" ", (1<<20)-len(empty)))
	if _, err := c.JobContext(t.Context(), "job-one", 20); err != nil {
		t.Fatal("legal1MiB", err)
	}
	c, _ = progressResponseClient(t, string(empty)+strings.Repeat(" ", (1<<20)+1-len(empty)))
	_, err := c.JobContext(t.Context(), "job-one", 20)
	checkError(t, err, "invalid_response")
}
func TestJobContextClientLocalValidationAndCancellation(t *testing.T) {
	for _, v := range []struct {
		id    string
		limit int
	}{{"../job", 20}, {"job-one", -1}, {"job-one", 101}} {
		_, err := (&client.Client{}).JobContext(t.Context(), v.id, v.limit)
		checkError(t, err, "validation_failed")
	}
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		requests.Add(1)
		<-r.Context().Done()
	}))
	defer service.Close()
	c := newClient(t, service)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err := c.JobContext(ctx, "job-one", 0)
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "deadline_exceeded" || failure.OutcomeUnknown || requests.Load() != 1 {
		t.Fatal("read cancellation/retry", err, requests.Load())
	}
}
