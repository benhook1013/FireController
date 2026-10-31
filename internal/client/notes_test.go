package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func notePtr(s string) *string { return &s }
func TestNotesClientPublicPartialCorrectionsHistoryAndReplay(t *testing.T) {
	f := newMutationFixture(t)
	var lose atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/notes") {
			writes.Add(1)
			if lose.CompareAndSwap(true, false) {
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, r)
				if rec.Code != 201 {
					t.Errorf("did not commit %d %s", rec.Code, rec.Body.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(rec.Code)
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	job, err := c.CreateJob(t.Context(), "notes-job", client.CreateJobRequest{Title: "Unchanged work", Brief: "Original", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	in := client.CreateNoteRequest{WorkerID: f.recipient, JobID: job.ID, Phase: "  Next phase  ", Kind: "source", Body: strings.Repeat("<", 16<<10)}
	// Preserving surrounding spaces is intentional.
	lose.Store(true)
	_, err = c.CreateNote(t.Context(), "note-lost", in)
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown || writes.Load() != 1 {
		t.Fatal("lost response retried", err, writes.Load())
	}
	original, err := c.CreateNote(t.Context(), "note-lost", in)
	if err != nil || original.Revision != 1 {
		t.Fatal(original, err)
	}
	full, err := c.Note(t.Context(), original.ID)
	if err != nil || full.Body != in.Body || full.Phase != in.Phase || full.WorkerID != f.recipient {
		t.Fatal("escaped full state", err)
	}
	page, err := c.Notes(t.Context(), client.PageOptions{Limit: 1}, client.NoteFilter{WorkerID: f.sender, IncludeAssignedJobs: true})
	if err != nil || len(page.Items) != 1 || !page.Items[0].BodyTruncated || page.Items[0].BodyPreview != strings.Repeat("<", 240) {
		t.Fatal(page, err)
	}
	direct, err := c.Notes(t.Context(), client.PageOptions{}, client.NoteFilter{WorkerID: f.sender})
	if err != nil || len(direct.Items) != 0 {
		t.Fatal("assignment expanded direct scope", direct, err)
	}
	dismissed, err := c.UpdateNote(t.Context(), "note-dismiss", original.ID, client.UpdateNoteRequest{ExpectedRevision: 1, Status: notePtr("dismissed"), StatusReason: notePtr("  Not needed\nnow  ")})
	if err != nil || dismissed.Revision != 2 {
		t.Fatal(dismissed, err)
	}
	changed, err := c.UpdateNote(t.Context(), "note-correction", original.ID, client.UpdateNoteRequest{ExpectedRevision: 2, Body: notePtr(" corrected body ")})
	if err != nil || changed.Revision != 3 {
		t.Fatal(changed, err)
	}
	full, err = c.Note(t.Context(), original.ID)
	if err != nil || full.StatusReason != "  Not needed\nnow  " || full.Status != "dismissed" {
		t.Fatal(full, err)
	}
	_, err = c.UpdateNote(t.Context(), "note-resume", original.ID, client.UpdateNoteRequest{ExpectedRevision: 3, Status: notePtr("pending")})
	if err != nil {
		t.Fatal(err)
	}
	effects := f.eventCount(t)
	noop, err := c.UpdateNote(t.Context(), "note-noop", original.ID, client.UpdateNoteRequest{ExpectedRevision: 4, Status: notePtr("pending")})
	if err != nil || noop.Outcome != "unchanged" || noop.Revision != 4 || f.eventCount(t) != effects {
		t.Fatal(noop, err)
	}
	_, err = c.UpdateNote(t.Context(), "note-stale", original.ID, client.UpdateNoteRequest{ExpectedRevision: 1, Body: notePtr(" corrected body ")})
	checkError(t, err, "note_revision_conflict")
	replay, err := c.CreateNote(t.Context(), "note-lost", in)
	if err != nil || replay != original || f.eventCount(t) != effects {
		t.Fatal("original replay changed", replay, err)
	}
	history, err := c.NoteHistory(t.Context(), original.ID, client.PageOptions{Limit: 1})
	if err != nil || len(history.Items) != 1 || history.Items[0].Revision != 4 || !history.HasMore || history.Items[0].ActorWorkerID != f.sender {
		t.Fatal(history, err)
	}
	previous, err := c.NoteHistory(t.Context(), original.ID, client.PageOptions{Limit: 1, Cursor: history.Cursor})
	if err != nil || previous.Items[0].Revision != 3 {
		t.Fatal(previous, err)
	}
	old, err := c.NoteRevision(t.Context(), original.ID, 1)
	if err != nil || old.Body != in.Body || old.Status != "pending" || old.StatusReason != "" {
		t.Fatal(old, err)
	}
	current, err := c.Note(t.Context(), original.ID)
	if err != nil || current.Body != " corrected body " || current.StatusReason != "" {
		t.Fatal(current, err)
	}
	work, err := c.Job(t.Context(), job.ID)
	if err != nil || work.Revision != 1 || work.Brief != "Original" || f.providerCalls.Load() != 1 || f.eventCount(t) != effects {
		t.Fatal("notes changed work or reads wrote", work, err)
	}
}
func TestNotesClientLocalValidationAndMalformedStates(t *testing.T) {
	// Keep client admission permissive for exact retries of historical receipts;
	// the service rejects a fresh phase-only mutation after replay lookup.
	if err := client.ValidateCreateNoteRequest(client.CreateNoteRequest{Phase: "Retained label", Kind: "reminder", Body: "legacy retry"}); err != nil {
		t.Fatal("client blocked a potentially retained create receipt", err)
	}
	if err := client.ValidateUpdateNoteRequest(client.UpdateNoteRequest{ExpectedRevision: 1, WorkerID: notePtr("")}); err != nil {
		t.Fatal("client preflighted partial scope without server state", err)
	}
	var c client.Client
	for _, in := range []client.CreateNoteRequest{{Kind: "reminder", Body: "x"}, {WorkerID: "worker", Kind: "other", Body: "x"}, {WorkerID: "worker", Kind: "source", Body: "\r"}, {WorkerID: "worker", Kind: "reminder", Body: string([]byte{0xff})}} {
		_, err := c.CreateNote(t.Context(), "k", in)
		checkError(t, err, "validation_failed")
	}
	for _, in := range []client.UpdateNoteRequest{{ExpectedRevision: 0, Status: notePtr("pending")}, {ExpectedRevision: math.MaxInt64, Body: notePtr("x")}} {
		_, err := c.UpdateNote(t.Context(), "k", "note", in)
		checkError(t, err, "invalid_revision")
	}
	for _, in := range []client.UpdateNoteRequest{{ExpectedRevision: 1}, {ExpectedRevision: 1, Status: notePtr("dismissed")}, {ExpectedRevision: 1, Status: notePtr("pending"), StatusReason: notePtr("wrong")}, {ExpectedRevision: 1, WorkerID: notePtr(""), JobID: notePtr(""), Phase: notePtr("")}} {
		_, err := c.UpdateNote(t.Context(), "k", "note", in)
		checkError(t, err, "validation_failed")
	}
	good := client.Note{ID: "note-one", ProjectID: "project-one", WorkerID: "worker-one", Kind: "reminder", Body: "text", Status: "pending", Revision: 1, CreatedAt: "2026-10-16T01:00:00Z", UpdatedAt: "2026-10-16T00:00:00Z"}
	// Observation clocks can move backwards; valid state must remain readable.
	response, _ := json.Marshal(good)
	cl, _ := progressResponseClient(t, string(response))
	if _, err := cl.Note(t.Context(), good.ID); err != nil {
		t.Fatal("invented chronology", err)
	}
	for _, raw := range []string{strings.Replace(string(response), `"status":"pending"`, `"status":"unknown"`, 1), strings.Replace(string(response), `"revision":1`, `"revision":1.0`, 1), strings.Replace(string(response), `"body":"text"`, `"body":null`, 1), strings.Replace(string(response), `"phase":""`, `"phase":"","phase":""`, 1), strings.Replace(string(response), `"project_id":"project-one"`, `"project_id":"foreign"`, 1), strings.Replace(string(response), `"body":"text"`, `"body":"\ud800"`, 1)} {
		cl, _ := progressResponseClient(t, raw)
		_, err := cl.Note(t.Context(), good.ID)
		checkError(t, err, "invalid_response")
	}
}
func TestNotesClientLargeCompactPageBudgetAndStrictShape(t *testing.T) {
	items := make([]client.NoteSummary, 100)
	history := make([]client.NoteHistorySummary, 100)
	for i := range items {
		items[i] = client.NoteSummary{ID: fmt.Sprintf("note-%03d", 999-i), ProjectID: "project-one", Revision: int64(100 - i), WorkerID: "worker-one", Phase: strings.Repeat("<", 200), Kind: "reminder", Status: "pending", CreatedAt: "2026-10-16T00:00:00Z", UpdatedAt: "2026-10-16T00:00:00Z", BodyPreview: strings.Repeat("<", 240), BodyTruncated: true}
		v := items[i]
		history[i] = client.NoteHistorySummary{ID: "note-one", ProjectID: v.ProjectID, Revision: v.Revision, WorkerID: v.WorkerID, Phase: v.Phase, Kind: v.Kind, Status: v.Status, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, BodyPreview: v.BodyPreview, BodyTruncated: true, ActorID: "actor-one", RecordedAt: v.UpdatedAt}
	}
	raw, _ := json.Marshal(client.Page[client.NoteSummary]{Items: items})
	if len(raw) <= 128<<10 || len(raw) > 512<<10 {
		t.Fatal(len(raw))
	}
	c, _ := progressResponseClient(t, string(raw))
	page, err := c.Notes(t.Context(), client.PageOptions{Limit: 100}, client.NoteFilter{})
	if err != nil || len(page.Items) != 100 {
		t.Fatal("legal expanded page rejected", err)
	}
	raw, _ = json.Marshal(client.Page[client.NoteHistorySummary]{Items: history})
	c, _ = progressResponseClient(t, string(raw))
	arch, err := c.NoteHistory(t.Context(), "note-one", client.PageOptions{Limit: 100})
	if err != nil || len(arch.Items) != 100 {
		t.Fatal("legal history rejected", err)
	}
	items[0].BodyPreview = "short"
	raw, _ = json.Marshal(client.Page[client.NoteSummary]{Items: items})
	c, _ = progressResponseClient(t, string(raw))
	_, err = c.Notes(t.Context(), client.PageOptions{Limit: 100}, client.NoteFilter{})
	checkError(t, err, "invalid_response")
}
func TestNotesClientCancellationNoRetryOrFanout(t *testing.T) {
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		reads.Add(1)
		<-r.Context().Done()
	}))
	defer service.Close()
	c := newClient(t, service)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err := c.Note(ctx, "note-one")
	checkError(t, err, "deadline_exceeded")
	if reads.Load() != 1 {
		t.Fatal("retry/fanout", reads.Load())
	}
	mutationCtx, mutationCancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer mutationCancel()
	_, err = c.CreateNote(mutationCtx, "uncertain-once", client.CreateNoteRequest{WorkerID: "worker-one", Kind: "reminder", Body: "bounded"})
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown || reads.Load() != 2 {
		t.Fatal("mutation cancellation lost uncertainty or retried", err, reads.Load())
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	_, err = c.CreateNote(canceled, "not-sent", client.CreateNoteRequest{WorkerID: "worker-one", Kind: "reminder", Body: "bounded"})
	if !errors.As(err, &unknown) || unknown.OutcomeUnknown || reads.Load() != 2 {
		t.Fatal("pre-canceled mutation sent", err, reads.Load())
	}
}

func TestNotesClientRequiresExplicitCompleteResponseFields(t *testing.T) {
	full := client.Note{ID: "note-one", ProjectID: "project-one", WorkerID: "worker-one", Kind: "reminder", Body: "body", Status: "pending", Revision: 1, CreatedAt: "2026-10-16T00:00:00.000000000Z", UpdatedAt: "2026-10-16T00:00:00.000000000Z"}
	summary := client.NoteSummary{ID: full.ID, ProjectID: full.ProjectID, WorkerID: full.WorkerID, Kind: full.Kind, Status: full.Status, Revision: full.Revision, CreatedAt: full.CreatedAt, UpdatedAt: full.UpdatedAt, BodyPreview: full.Body, BodyTruncated: false}
	revision := client.NoteRevision{ID: full.ID, ProjectID: full.ProjectID, WorkerID: full.WorkerID, Kind: full.Kind, Body: full.Body, Status: full.Status, Revision: full.Revision, CreatedAt: full.CreatedAt, UpdatedAt: full.UpdatedAt, ActorID: "actor-one", RecordedAt: full.UpdatedAt}
	historic := client.NoteHistorySummary{ID: summary.ID, ProjectID: summary.ProjectID, WorkerID: summary.WorkerID, Kind: summary.Kind, Status: summary.Status, Revision: summary.Revision, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt, BodyPreview: summary.BodyPreview, BodyTruncated: false, ActorID: "actor-one", RecordedAt: summary.UpdatedAt}
	for _, null := range []bool{false, true} {
		for _, v := range []struct {
			name       string
			value      any
			field      string
			page, item bool
			read       func(*client.Client) error
		}{
			{"current worker", full, "worker_id", false, false, func(c *client.Client) error { _, e := c.Note(t.Context(), full.ID); return e }},
			{"current statusreason", full, "status_reason", false, false, func(c *client.Client) error { _, e := c.Note(t.Context(), full.ID); return e }},
			{"revision actor", revision, "actor_worker_id", false, false, func(c *client.Client) error { _, e := c.NoteRevision(t.Context(), full.ID, 1); return e }},
			{"page hasmore", client.Page[client.NoteSummary]{Items: []client.NoteSummary{summary}}, "has_more", true, false, func(c *client.Client) error {
				_, e := c.Notes(t.Context(), client.PageOptions{}, client.NoteFilter{})
				return e
			}},
			{"page items", client.Page[client.NoteSummary]{Items: []client.NoteSummary{summary}}, "items", true, false, func(c *client.Client) error {
				_, e := c.Notes(t.Context(), client.PageOptions{}, client.NoteFilter{})
				return e
			}},
			{"preview completeflag", client.Page[client.NoteSummary]{Items: []client.NoteSummary{summary}}, "body_truncated", true, true, func(c *client.Client) error {
				_, e := c.Notes(t.Context(), client.PageOptions{}, client.NoteFilter{})
				return e
			}},
			{"preview scope", client.Page[client.NoteSummary]{Items: []client.NoteSummary{summary}}, "phase", true, true, func(c *client.Client) error {
				_, e := c.Notes(t.Context(), client.PageOptions{}, client.NoteFilter{})
				return e
			}},
			{"history completeflag", client.Page[client.NoteHistorySummary]{Items: []client.NoteHistorySummary{historic}}, "body_truncated", true, true, func(c *client.Client) error {
				_, e := c.NoteHistory(t.Context(), full.ID, client.PageOptions{})
				return e
			}},
			{"history hasmore", client.Page[client.NoteHistorySummary]{Items: []client.NoteHistorySummary{historic}}, "has_more", true, false, func(c *client.Client) error {
				_, e := c.NoteHistory(t.Context(), full.ID, client.PageOptions{})
				return e
			}},
		} {
			t.Run(fmt.Sprintf("%s/null=%v", v.name, null), func(t *testing.T) {
				raw, _ := json.Marshal(v.value)
				var obj map[string]json.RawMessage
				json.Unmarshal(raw, &obj)
				target := obj
				var items []map[string]json.RawMessage
				if v.item {
					json.Unmarshal(obj["items"], &items)
					target = items[0]
				}
				if null {
					target[v.field] = json.RawMessage("null")
				} else {
					delete(target, v.field)
				}
				if v.item {
					obj["items"], _ = json.Marshal(items)
				}
				raw, _ = json.Marshal(obj)
				c, _ := progressResponseClient(t, string(raw))
				err := v.read(c)
				checkError(t, err, "invalid_response")
				var failure *client.Error
				if !errors.As(err, &failure) || failure.OutcomeUnknown {
					t.Fatal("read became mutation uncertainty", err)
				}
			})
		}
	}
	raw, _ := json.Marshal(client.Page[client.NoteSummary]{Items: []client.NoteSummary{summary}})
	c, _ := progressResponseClient(t, string(raw))
	if _, err := c.Notes(t.Context(), client.PageOptions{}, client.NoteFilter{}); err != nil {
		t.Fatal("explicit false rejected", err)
	}
	var cursorObject map[string]json.RawMessage
	json.Unmarshal(raw, &cursorObject)
	cursorObject["cursor"] = json.RawMessage("null")
	raw, _ = json.Marshal(cursorObject)
	c, _ = progressResponseClient(t, string(raw))
	_, err := c.Notes(t.Context(), client.PageOptions{}, client.NoteFilter{})
	checkError(t, err, "invalid_response")
}

func TestNoteBodyValidationUsesDecodedUTF8Bytes(t *testing.T) {
	good := strings.Repeat("雪", client.MaxNoteBodyBytes/3) + "x"
	if len(good) != client.MaxNoteBodyBytes {
		t.Fatal("invalid exact-byte fixture")
	}
	if err := client.ValidateCreateNoteRequest(client.CreateNoteRequest{WorkerID: "worker-one", Kind: "reminder", Body: good}); err != nil {
		t.Fatal(err)
	}
	if err := client.ValidateUpdateNoteRequest(client.UpdateNoteRequest{ExpectedRevision: 1, Body: &good}); err != nil {
		t.Fatal(err)
	}
	bad := "PRIVATE_NOTE_BODY" + strings.Repeat("雪", client.MaxNoteBodyBytes/3+1)
	for _, err := range []error{
		client.ValidateCreateNoteRequest(client.CreateNoteRequest{WorkerID: "worker-one", Kind: "reminder", Body: bad}),
		client.ValidateUpdateNoteRequest(client.UpdateNoteRequest{ExpectedRevision: 1, Body: &bad}),
	} {
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "validation_failed" || failure.OutcomeUnknown || !strings.Contains(failure.Message, "body exceeds 16384 decoded UTF-8 bytes") || !strings.Contains(failure.Action, "omit body when unchanged") {
			t.Fatal("missing body byte diagnostic", err)
		}
		raw, _ := json.Marshal(failure)
		if strings.Contains(string(raw), "PRIVATE_NOTE_BODY") || !strings.Contains(string(raw), `"field":"body"`) {
			t.Fatal("body leaked or field omitted", string(raw))
		}
	}
}
