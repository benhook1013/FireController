package client_test

import (
	"context"
	"errors"
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

func TestPrimaryJobClientPublicReplayAndIndependentFocus(t *testing.T) {
	f := newMutationFixture(t)
	var lose atomic.Bool
	var replacements atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/primary-job") {
			replacements.Add(1)
			if lose.CompareAndSwap(true, false) {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != 200 {
					t.Errorf("replacement did not commit: %d %s", actual.Code, actual.Body.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(actual.Code)
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	firstJob, err := c.CreateJob(t.Context(), "primary-job-one", client.CreateJobRequest{Title: "First active job", Brief: "Standing brief", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := c.CreateJob(t.Context(), "primary-job-two", client.CreateJobRequest{Title: "Second active job", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := c.PrimaryJob(t.Context())
	if err != nil || empty.WorkerID != f.sender || empty.Revision != 0 || empty.JobID != "" {
		t.Fatal(empty, err)
	}
	noChange, err := c.ReplacePrimaryJob(t.Context(), "initial-clear", client.ReplacePrimaryJobRequest{})
	if err != nil || noChange.Revision != 0 || noChange.Outcome != "unchanged" {
		t.Fatal(noChange, err)
	}
	request := client.ReplacePrimaryJobRequest{ExpectedRevision: 0, JobID: firstJob.ID}
	lose.Store(true)
	_, err = c.ReplacePrimaryJob(t.Context(), "lost-primary", request)
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown || replacements.Load() != 2 {
		t.Fatal("lost response retried or lost uncertainty", err)
	}
	original, err := c.ReplacePrimaryJob(t.Context(), "lost-primary", request)
	if err != nil || original.ID != f.sender || original.Revision != 1 || original.Outcome != "updated" {
		t.Fatal(original, err)
	}
	switched, err := c.ReplacePrimaryJob(t.Context(), "switch-primary", client.ReplacePrimaryJobRequest{ExpectedRevision: 1, JobID: secondJob.ID})
	if err != nil || switched.Revision != 2 {
		t.Fatal(switched, err)
	}
	current, err := c.PrimaryJob(t.Context())
	if err != nil || current.JobID != secondJob.ID || current.Revision != 2 {
		t.Fatal(current, err)
	}
	before := f.eventCount(t)
	replay, err := c.ReplacePrimaryJob(t.Context(), "lost-primary", request)
	if err != nil || replay != original || f.eventCount(t) != before {
		t.Fatal("exact-key replay changed original receipt/effects", replay, err)
	}
	_, err = c.ReplacePrimaryJob(t.Context(), "lost-primary", client.ReplacePrimaryJobRequest{JobID: secondJob.ID})
	checkError(t, err, "idempotency_conflict")
	history, err := c.PrimaryJobHistory(t.Context(), client.PageOptions{Limit: 1})
	if err != nil || len(history.Items) != 1 || !history.HasMore || history.Items[0].Revision != 2 || history.Items[0].JobID != secondJob.ID || history.Items[0].ActorWorkerID != f.sender {
		t.Fatal(history, err)
	}
	older, err := c.PrimaryJobHistory(t.Context(), client.PageOptions{Limit: 1, Cursor: history.Cursor})
	if err != nil || len(older.Items) != 1 || older.HasMore || older.Items[0].Revision != 1 || older.Items[0].JobID != firstJob.ID {
		t.Fatal(older, err)
	}
	state, err := c.Job(t.Context(), firstJob.ID)
	if err != nil || state.Revision != 1 || state.Status != "active" || state.WorkerID != f.sender || state.Brief != "Standing brief" {
		t.Fatal("focus edited job", state, err)
	}
	cleared, err := c.ReplacePrimaryJob(t.Context(), "clear-primary", client.ReplacePrimaryJobRequest{ExpectedRevision: 2, JobID: ""})
	if err != nil || cleared.Revision != 3 {
		t.Fatal(cleared, err)
	}
	current, err = c.PrimaryJob(t.Context())
	if err != nil || current.JobID != "" || current.Revision != 3 {
		t.Fatal(current, err)
	}
	if f.providerCalls.Load() != 1 {
		t.Fatal("primary made provider calls")
	}
}

func TestPrimaryJobClientStrictResponsesAndLocalValidation(t *testing.T) {
	for _, body := range []string{
		`{"worker_id":"worker-other","revision":0,"job_id":""}`,
		`{"worker_id":"worker-one","revision":0,"job_id":"job-one"}`,
		`{"worker_id":"worker-one","revision":-1,"job_id":""}`,
		`{"worker_id":"worker-one","revision":9223372036854775808,"job_id":""}`,
		`{"worker_id":"worker-one","revision":0.0,"job_id":""}`,
		`{"worker_id":"worker-one","revision":0,"job_id":null}`,
		`{"worker_id":"worker-one","revision":0}`,
		`{"worker_id":"worker-one","revision":0,"job_id":"","JobID":""}`,
		`{"worker_id":"worker-one","revision":0,"job_id":"","job_id":""}`,
		`{"worker_id":"worker-one","revision":1,"job_id":"bad/job"}`,
	} {
		c, _ := progressResponseClient(t, body)
		_, err := c.PrimaryJob(t.Context())
		checkError(t, err, "invalid_response")
	}
	for _, revision := range []int64{0, math.MaxInt64} {
		body := progressJSON(t, client.PrimaryJob{WorkerID: "worker-one", Revision: revision})
		c, _ := progressResponseClient(t, body)
		if got, err := c.PrimaryJob(t.Context()); err != nil || got.Revision != revision {
			t.Fatal(got, err)
		}
	}
	entry := client.PrimaryJobHistoryEntry{WorkerID: "worker-one", Revision: 2, JobID: "job-one", ActorID: "principal-one", Reason: "selected", CreatedAt: "2026-10-15T00:00:00Z"}
	for _, change := range []func(*client.PrimaryJobHistoryEntry){func(v *client.PrimaryJobHistoryEntry) { v.WorkerID = "other" }, func(v *client.PrimaryJobHistoryEntry) { v.Revision = 0 }, func(v *client.PrimaryJobHistoryEntry) { v.JobID = "" }, func(v *client.PrimaryJobHistoryEntry) { v.Reason = "cleared" }, func(v *client.PrimaryJobHistoryEntry) { v.Reason = "unknown" }, func(v *client.PrimaryJobHistoryEntry) { v.ActorID = "" }, func(v *client.PrimaryJobHistoryEntry) { v.ActorWorkerID = "../other" }, func(v *client.PrimaryJobHistoryEntry) { v.CreatedAt = "0001-01-01T00:00:00Z" }} {
		bad := entry
		change(&bad)
		c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.PrimaryJobHistoryEntry]{Items: []client.PrimaryJobHistoryEntry{bad}}))
		_, err := c.PrimaryJobHistory(t.Context(), client.PageOptions{})
		checkError(t, err, "invalid_response")
	}
	for _, entries := range [][]client.PrimaryJobHistoryEntry{{entry, entry}, {entry, {WorkerID: "worker-one", Revision: 3, JobID: "job-two", ActorID: "principal-one", Reason: "selected", CreatedAt: entry.CreatedAt}}} {
		c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.PrimaryJobHistoryEntry]{Items: entries}))
		_, err := c.PrimaryJobHistory(t.Context(), client.PageOptions{})
		checkError(t, err, "invalid_response")
	}
	c, calls := progressResponseClient(t, `{}`)
	for _, r := range []client.ReplacePrimaryJobRequest{{ExpectedRevision: -1}, {ExpectedRevision: math.MaxInt64}, {JobID: "bad/job"}, {JobID: " padded"}} {
		if _, err := c.ReplacePrimaryJob(t.Context(), "explicit", r); err == nil {
			t.Fatal("invalid request admitted")
		}
	}
	if _, err := c.ReplacePrimaryJob(t.Context(), "", client.ReplacePrimaryJobRequest{}); err == nil {
		t.Fatal("empty key admitted")
	}
	if _, err := c.PrimaryJobHistory(t.Context(), client.PageOptions{Limit: 101}); err == nil {
		t.Fatal("invalid limit admitted")
	}
	if _, err := c.PrimaryJobHistory(t.Context(), client.PageOptions{Cursor: " padded"}); err == nil {
		t.Fatal("invalid cursor admitted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid local input sent HTTP", calls.Load())
	}
}

func TestPrimaryJobClientRequestScopeCancellationAndUnknownReceipt(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.URL.Path != "/api/v1/projects/project-one/workers/worker-one/primary-job" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" {
			t.Error("bound selector/header lost")
		}
		if r.Method == "PUT" {
			write(w, 200, map[string]any{"id": "other-worker", "revision": 1, "outcome": "updated"})
			return
		}
		<-r.Context().Done()
	}))
	defer service.Close()
	c := newClient(t, service)
	_, err := c.ReplacePrimaryJob(t.Context(), "explicit", client.ReplacePrimaryJobRequest{JobID: "job-one"})
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown || calls.Load() != 1 {
		t.Fatal("invalid dispatched receipt lost uncertainty/retried", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err = c.PrimaryJob(ctx)
	if !errors.As(err, &unknown) || unknown.Code != "deadline_exceeded" || unknown.OutcomeUnknown {
		t.Fatal("read cancellation outcome", err)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected fanout", calls.Load())
	}
}

func TestPrimaryJobClientZeroConflictDetailsAreCodeScoped(t *testing.T) {
	for _, code := range []string{"primary_job_revision_conflict", "revision_conflict"} {
		t.Run(code, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 409, map[string]any{"error": map[string]any{"code": code, "message": "Stale guarded revision.", "details": map[string]any{"current_revision": 0}}, "request_id": "request-one"})
			}))
			defer s.Close()
			c := newClient(t, s)
			_, err := c.ReplacePrimaryJob(t.Context(), "stale", client.ReplacePrimaryJobRequest{ExpectedRevision: 1})
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != code || failure.OutcomeUnknown {
				t.Fatal("known conflict lost", err)
			}
			if code == "primary_job_revision_conflict" {
				details, ok := failure.Details.(map[string]any)
				if !ok || details["current_revision"] != int64(0) {
					t.Fatal("valid zero focus revision lost", failure.Details)
				}
			} else if failure.Details != nil {
				t.Fatal("other revision policies expanded", failure.Details)
			}
		})
	}
}

func TestChecklistZeroRevisionConflictPreservesKnownCurrentState(t *testing.T) {
	f := newMutationFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	job, err := c.CreateJob(t.Context(), "zero-checklist-job", client.CreateJobRequest{Title: "No checklist yet"})
	if err != nil {
		t.Fatal(err)
	}
	before := f.eventCount(t)
	_, err = c.ReplaceJobChecklist(t.Context(), "stale-initial-checklist", job.ID, client.ReplaceChecklistRequest{ExpectedJobRevision: 1, ExpectedRevision: 1, Items: []client.ChecklistItem{}})
	var failure *client.Error
	if !errors.As(err, &failure) || failure.Code != "checklist_revision_conflict" || failure.OutcomeUnknown {
		t.Fatal("known checklist conflict changed", err)
	}
	details, ok := failure.Details.(map[string]any)
	if !ok || details["current_revision"] != int64(0) {
		t.Fatal("initial checklist conflict lost currentrevisionzero", failure.Details)
	}
	current, err := c.JobChecklist(t.Context(), job.ID)
	if err != nil || current.Revision != 0 || current.Items == nil || len(current.Items) != 0 || f.eventCount(t) != before {
		t.Fatal("stalechecklist changed currentstate", current, err)
	}
}
