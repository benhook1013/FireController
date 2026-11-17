package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestUnifiedJobClientSnapshotPartialProgressNoopAndReplay(t *testing.T) {
	f := newMutationFixture(t)
	s := httptest.NewServer(f.handler)
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	job, err := c.CreateJob(t.Context(), "state-job", client.CreateJobRequest{Title: "Standing job", Brief: "Private instructions", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.CreateJob(t.Context(), "other-job", client.CreateJobRequest{Title: "Other", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		_, err = c.CreateNote(t.Context(), "note-key-"+string(rune('a'+i)), client.CreateNoteRequest{WorkerID: f.recipient, JobID: job.ID, Kind: "reminder", Body: "Exact job pending"})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = c.CreateNote(t.Context(), "unrelated-note", client.CreateNoteRequest{WorkerID: f.sender, JobID: other.ID, Kind: "reminder", Body: "Must stay out"})
	if err != nil {
		t.Fatal(err)
	}
	plan := []client.ChecklistItem{{ID: "one", Text: "Remaining"}, {ID: "two", Text: "Finished", Done: true}}
	if _, err = c.ReplaceJobChecklist(t.Context(), "checklist", job.ID, client.ReplaceChecklistRequest{ExpectedJobRevision: 1, Items: plan}); err != nil {
		t.Fatal(err)
	}
	summary, next, blocker := "<literal>\n"+strings.Repeat("雪", 100), "Next", "Blocked"
	evidence := []string{"proof:one"}
	patch := client.UpdateJobRequest{ExpectedRevision: 1, Progress: &client.ProgressPatch{Summary: &summary, Next: &next, Blocker: &blocker, Evidence: &evidence}}
	first, err := c.UpdateJob(t.Context(), "once", job.ID, patch)
	if err != nil || first.Revision != 2 {
		t.Fatal(first, err)
	}
	if replay, err := c.UpdateJob(t.Context(), "once", job.ID, patch); err != nil || replay != first {
		t.Fatal("exact replay changed", err)
	}
	before := f.eventCount(t)
	current, err := c.Job(t.Context(), job.ID)
	if err != nil || current.Revision != 2 || current.Progress.Summary != summary || current.Brief != "Private instructions" || len(current.Notes.Items) != 5 || !current.Notes.HasMore || len(current.Checklist.Remaining) != 1 || current.Checklist.Completed != 1 || current.Checklist.Total != 2 {
		t.Fatal("snapshot incomplete", err)
	}
	if f.eventCount(t) != before {
		t.Fatal("read mutated events")
	}
	notes, err := c.Notes(t.Context(), client.PageOptions{Limit: 5, Cursor: current.Notes.Cursor}, client.NoteFilter{JobID: job.ID, Status: "pending"})
	if err != nil || len(notes.Items) != 1 || notes.HasMore {
		t.Fatal("exact-job cursor incompatible", err)
	}
	empty := ""
	clear := []string{}
	second, err := c.UpdateJob(t.Context(), "clear", job.ID, client.UpdateJobRequest{ExpectedRevision: 2, Progress: &client.ProgressPatch{Summary: &empty, Evidence: &clear}})
	if err != nil || second.Revision != 3 {
		t.Fatal(second, err)
	}
	current, err = c.Job(t.Context(), job.ID)
	if err != nil || current.Progress.Summary != "" || current.Progress.Next != "Next" || current.Progress.Blocker != "Blocked" || len(current.Progress.Evidence) != 0 {
		t.Fatal("partial replacement changed omitted fields", err)
	}
	before = f.eventCount(t)
	noop, err := c.UpdateJob(t.Context(), "noop", job.ID, client.UpdateJobRequest{ExpectedRevision: 3, Progress: &client.ProgressPatch{Summary: &empty}})
	if err != nil || noop.Revision != 3 || noop.Outcome != "unchanged" || f.eventCount(t) != before {
		t.Fatal("noop effects", err)
	}
	replay, err := c.UpdateJob(t.Context(), "once", job.ID, patch)
	if err != nil || replay != first {
		t.Fatal("original receipt rewritten", err)
	}
	page, err := c.JobProgressHistory(t.Context(), job.ID, client.PageOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || !page.HasMore || page.Items[0].JobRevision != 3 {
		t.Fatal("one archive page", err)
	}
	page, err = c.JobProgressHistory(t.Context(), job.ID, client.PageOptions{Limit: 1, Cursor: page.Cursor})
	if err != nil || len(page.Items) != 1 || page.Items[0].Progress.Summary != summary {
		t.Fatal("history overwritten", err)
	}
	list, err := c.Jobs(t.Context(), client.PageOptions{}, client.JobFilter{})
	if err != nil || len(list.Items) != 2 {
		t.Fatal("compact list", err)
	}
	_, err = c.UpdateJob(t.Context(), "stale", job.ID, patch)
	checkError(t, err, "revision_conflict")
	if f.providerCalls.Load() != 1 {
		t.Fatal("job calls used provider")
	}
}
func validCurrentJob() client.Job {
	return client.Job{ID: "job-one", ProjectID: "project-one", Title: "Job", Brief: "Full brief", Status: "active", Revision: 1, CreatedAt: "2026-10-21T00:00:00Z", UpdatedAt: "2026-10-20T00:00:00Z", Progress: client.Progress{Evidence: []string{}}, Checklist: client.JobChecklistSummary{Remaining: []client.ChecklistItem{}}, Notes: client.Page[client.NoteSummary]{Items: []client.NoteSummary{}}}
}
func TestUnifiedJobClientStrictResponsesAndReadOnlyArchive(t *testing.T) {
	good := validCurrentJob()
	encoded := progressJSON(t, good)
	cases := []string{strings.Replace(encoded, `"summary":""`, `"summary":null`, 1), strings.Replace(encoded, `"evidence":[]`, `"evidence":null`, 1), strings.Replace(encoded, `"completed":0,`, "", 1), strings.Replace(encoded, `"has_more":false`, `"has_more":null`, 1), strings.Replace(encoded, `"has_more":false`, `"has_more":false,"inbox":[]`, 1), strings.Replace(encoded, `"project_id":"project-one"`, `"project_id":"other"`, 1), strings.Replace(encoded, `"updated_at":"2026-10-20T00:00:00Z"`, `"updated_at":"malformed"`, 1), strings.Replace(encoded, `"total":0`, `"total":1`, 1), strings.Replace(encoded, `"notes":`, `"notes":null,"notes":`, 1)}
	for i, body := range cases {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			c, n := progressResponseClient(t, body)
			_, err := c.Job(t.Context(), "job-one")
			checkError(t, err, "invalid_response")
			if n.Load() != 1 {
				t.Fatal("read fanout")
			}
		})
	}
	c, n := progressResponseClient(t, encoded)
	got, err := c.Job(t.Context(), "job-one")
	if err != nil || !reflect.DeepEqual(got, good) || n.Load() != 1 {
		t.Fatal("valid literal/current backward clock refused", err)
	}
	// Legacy history may omit progress, but current reads cannot.
	legacy := client.JobState{ID: good.ID, ProjectID: good.ProjectID, Title: good.Title, Brief: good.Brief, Status: good.Status, Revision: 1, CreatedAt: good.CreatedAt, UpdatedAt: good.UpdatedAt}
	c, _ = progressResponseClient(t, progressJSON(t, client.Page[client.JobHistoryEntry]{Items: []client.JobHistoryEntry{{Revision: 1, ActorID: "actor-one", State: legacy, CreatedAt: good.UpdatedAt}}}))
	if _, err = c.JobHistory(t.Context(), "job-one", client.PageOptions{}); err != nil {
		t.Fatal("legacy archive absence refused", err)
	}
	missing := strings.Replace(encoded, `"progress":{"summary":"","next":"","blocker":"","evidence":[]},`, "", 1)
	c, _ = progressResponseClient(t, missing)
	_, err = c.Job(t.Context(), "job-one")
	checkError(t, err, "invalid_response")
}
func TestUnifiedJobClientLocalPatchAndCancellation(t *testing.T) {
	cases := []string{`{"expected_revision":1}`, `{"expected_revision":0,"brief":"x"}`, `{"expected_revision":1,"progress":{}}`, `{"expected_revision":1,"progress":null}`, `{"expected_revision":1,"progress":{"summary":null}}`, `{"expected_revision":1,"progress":{"evidence":[null]}}`, `{"expected_revision":1,"progress":{"summary":"one","summary":"two"}}`, `{"expected_revision":1,"progress":{"summary":"bad\ud800"}}`, `{"expected_revision":1,"progress":{"unexpected":"x"}}`, `{"expected_revision":1,"worker_id":null}`}
	for _, raw := range cases {
		if _, err := client.DecodeJobPatch([]byte(raw)); err == nil {
			t.Fatal("invalid patch accepted")
		}
	}
	max := strings.Repeat("&", 8<<10)
	next := strings.Repeat("x", 8<<10)
	valid := client.UpdateJobRequest{ExpectedRevision: 1, Progress: &client.ProgressPatch{Summary: &max, Next: &next}}
	if client.ValidateJobPatch(valid) != nil {
		t.Fatal("exact aggregate refused")
	}
	blocker := "x"
	valid.Progress.Blocker = &blocker
	if client.ValidateJobPatch(valid) == nil {
		t.Fatal("aggregate overflow accepted")
	}
	valid.Progress.Blocker = nil
	valid.Progress.Next = nil
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (&client.Client{}).UpdateJob(canceled, "key", "job-one", client.UpdateJobRequest{ExpectedRevision: 0, Progress: valid.Progress})
	checkError(t, err, "invalid_revision")
	// The shared ordinary request budget remains a cancellable read budget.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		<-r.Context().Done()
	}))
	defer s.Close()
	c := newClient(t, s)
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer stop()
	_, err = c.Job(ctx, "job-one")
	var ce *client.Error
	if !errors.As(err, &ce) || ce.Code != "deadline_exceeded" || ce.OutcomeUnknown {
		t.Fatal("read cancellation became uncertain", err)
	}
}
func TestUnifiedJobClientInvalidReceiptIsUnknown(t *testing.T) {
	c, n := progressResponseClient(t, `{"id":"job-one","revision":9,"outcome":"updated"}`)
	text := "progress"
	_, err := c.UpdateJob(t.Context(), "key", "job-one", client.UpdateJobRequest{ExpectedRevision: 1, Progress: &client.ProgressPatch{Summary: &text}})
	var ce *client.Error
	if !errors.As(err, &ce) || ce.Code != "invalid_response" || !ce.OutcomeUnknown || n.Load() != 1 {
		t.Fatal("invalid dispatched receipt not unknown", err)
	}
}
