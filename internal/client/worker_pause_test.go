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

func TestWorkerPauseClientPublicStateReplayAndIndependentCoordination(t *testing.T) {
	f := newMutationFixture(t)
	var lose atomic.Bool
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/pause") {
			writes.Add(1)
			if lose.CompareAndSwap(true, false) {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != 200 {
					t.Errorf("pause failed to commit: %d", actual.Code)
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
	job, err := c.CreateJob(t.Context(), "pause-job", client.CreateJobRequest{Title: "Independent active work", Brief: "Retained brief", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReplacePrimaryJob(t.Context(), "pause-focus", client.ReplacePrimaryJobRequest{JobID: job.ID}); err != nil {
		t.Fatal(err)
	}
	empty, err := c.WorkerPause(t.Context())
	if err != nil || empty.WorkerID != f.sender || empty.Revision != 0 || empty.Paused || empty.Reason != "" {
		t.Fatal(empty, err)
	}
	_, err = c.ReplaceWorkerPause(t.Context(), "pause-stale-zero", client.ReplaceWorkerPauseRequest{ExpectedRevision: 1})
	var stale *client.Error
	if !errors.As(err, &stale) || stale.Code != "worker_pause_revision_conflict" || stale.OutcomeUnknown || stale.Details.(map[string]any)["current_revision"] != int64(0) {
		t.Fatal("lost authoritative zero revision", err)
	}
	initial, err := c.ReplaceWorkerPause(t.Context(), "pause-noop", client.ReplaceWorkerPauseRequest{})
	if err != nil || initial.Revision != 0 || initial.Outcome != "unchanged" {
		t.Fatal(initial, err)
	}
	request := client.ReplaceWorkerPauseRequest{Paused: true, Reason: strings.Repeat("<", 1024)}
	before := writes.Load()
	lose.Store(true)
	_, err = c.ReplaceWorkerPause(t.Context(), "pause-lost", request)
	var unknown *client.Error
	if !errors.As(err, &unknown) || !unknown.OutcomeUnknown || writes.Load() != before+1 {
		t.Fatal("lost committed response was retried or acknowledged", err)
	}
	original, err := c.ReplaceWorkerPause(t.Context(), "pause-lost", request)
	if err != nil || original.ID != f.sender || original.Revision != 1 || original.Outcome != "updated" {
		t.Fatal(original, err)
	}
	state, err := c.WorkerPause(t.Context())
	if err != nil || !state.Paused || state.Reason != request.Reason || state.Revision != 1 {
		t.Fatal("escaped reason did not roundtrip", state, err)
	}
	corrected, err := c.ReplaceWorkerPause(t.Context(), "pause-reason", client.ReplaceWorkerPauseRequest{ExpectedRevision: 1, Paused: true, Reason: "  waiting\tfor review\nnext step  "})
	if err != nil || corrected.Revision != 2 {
		t.Fatal(corrected, err)
	}
	resumed, err := c.ReplaceWorkerPause(t.Context(), "pause-resume", client.ReplaceWorkerPauseRequest{ExpectedRevision: 2})
	if err != nil || resumed.Revision != 3 {
		t.Fatal(resumed, err)
	}
	effects := f.eventCount(t)
	replay, err := c.ReplaceWorkerPause(t.Context(), "pause-lost", request)
	if err != nil || replay != original || f.eventCount(t) != effects {
		t.Fatal("original receipt changed after resume", replay, err)
	}
	newest, err := c.WorkerPauseHistory(t.Context(), client.PageOptions{Limit: 1})
	if err != nil || len(newest.Items) != 1 || newest.Items[0].Revision != 3 || newest.Items[0].Paused || newest.Items[0].Reason != "" || !newest.HasMore {
		t.Fatal(newest, err)
	}
	middle, err := c.WorkerPauseHistory(t.Context(), client.PageOptions{Limit: 1, Cursor: newest.Cursor})
	if err != nil || len(middle.Items) != 1 || middle.Items[0].Revision != 2 || middle.Items[0].Reason != "  waiting\tfor review\nnext step  " || middle.Items[0].ActorWorkerID != f.sender {
		t.Fatal(middle, err)
	}
	focus, err := c.PrimaryJob(t.Context())
	if err != nil || focus.JobID != job.ID || focus.Revision != 1 {
		t.Fatal("pause changed focus", focus, err)
	}
	work, err := c.Job(t.Context(), job.ID)
	if err != nil || work.Revision != 1 || work.Status != "active" || work.Brief != "Retained brief" || work.WorkerID != f.sender {
		t.Fatal("pause changed job", work, err)
	}
	other := f.newClient(t, f.admin, f.recipient, service.URL)
	untouched, err := other.WorkerPause(t.Context())
	if err != nil || untouched.Revision != 0 || untouched.Paused {
		t.Fatal("pause crossed worker scope", untouched, err)
	}
	if f.eventCount(t) != effects || f.providerCalls.Load() != 1 {
		t.Fatal("pause reads mutated state or called provider")
	}
}

func TestWorkerPauseClientStrictShapeAndLocalValidation(t *testing.T) {
	valid := `{"worker_id":"worker-one","revision":1,"paused":true,"reason":"waiting"}`
	for _, body := range []string{
		strings.Replace(valid, "worker-one", "worker-other", 1), strings.Replace(valid, `"revision":1`, `"revision":-1`, 1), strings.Replace(valid, `"revision":1`, `"revision":0`, 1), strings.Replace(valid, `"revision":1`, `"revision":1.0`, 1),
		strings.Replace(valid, `"paused":true,`, "", 1), strings.Replace(valid, `"paused":true`, `"paused":null`, 1), strings.Replace(valid, `"paused":true`, `"paused":false`, 1), strings.Replace(valid, `"reason":"waiting"`, `"reason":null`, 1), strings.Replace(valid, `"reason":"waiting"`, `"reason":"\u0085"`, 1), strings.Replace(valid, `"reason":"waiting"`, `"reason":"\r"`, 1), strings.Replace(valid, `"reason":"waiting"`, `"reason":"\ud800"`, 1), strings.Replace(valid, `"reason":"waiting"`, `"reason":"waiting","Reason":"waiting"`, 1), strings.Replace(valid, `"reason":"waiting"`, `"reason":"waiting","reason":"waiting"`, 1),
	} {
		c, _ := progressResponseClient(t, body)
		_, err := c.WorkerPause(t.Context())
		checkError(t, err, "invalid_response")
	}
	for _, revision := range []int64{0, math.MaxInt64} {
		c, _ := progressResponseClient(t, progressJSON(t, client.WorkerPause{WorkerID: "worker-one", Revision: revision}))
		if got, err := c.WorkerPause(t.Context()); err != nil || got.Revision != revision {
			t.Fatal(got, err)
		}
	}
	c, requests := progressResponseClient(t, valid)
	for _, request := range []client.ReplaceWorkerPauseRequest{{ExpectedRevision: -1}, {ExpectedRevision: math.MaxInt64}, {Reason: "not paused"}, {Paused: true, Reason: strings.Repeat("界", 342)}, {Paused: true, Reason: "\r"}, {Paused: true, Reason: "\u0085"}, {Paused: true, Reason: string([]byte{0xff})}} {
		if _, err := c.ReplaceWorkerPause(t.Context(), "explicit", request); err == nil {
			t.Fatal("invalid pause admitted", request)
		}
	}
	for _, options := range []client.PageOptions{{Limit: 101}, {Cursor: " bad"}, {Cursor: "\n"}} {
		if _, err := c.WorkerPauseHistory(t.Context(), options); err == nil {
			t.Fatal("invalid page admitted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input dispatched", requests.Load())
	}
}

func TestWorkerPauseClientRejectsMalformedHistoryAndReceipts(t *testing.T) {
	row := client.WorkerPauseHistoryEntry{WorkerID: "worker-one", Revision: 2, Paused: true, Reason: "waiting", ActorID: "actor-one", CreatedAt: "2026-10-16T01:02:03Z"}
	for _, change := range []func(*client.WorkerPauseHistoryEntry){func(r *client.WorkerPauseHistoryEntry) { r.WorkerID = "worker-other" }, func(r *client.WorkerPauseHistoryEntry) { r.Revision = 0 }, func(r *client.WorkerPauseHistoryEntry) { r.Paused = false }, func(r *client.WorkerPauseHistoryEntry) { r.ActorID = "" }, func(r *client.WorkerPauseHistoryEntry) { r.CreatedAt = "bad" }} {
		changed := row
		change(&changed)
		c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.WorkerPauseHistoryEntry]{Items: []client.WorkerPauseHistoryEntry{changed}}))
		_, err := c.WorkerPauseHistory(t.Context(), client.PageOptions{})
		checkError(t, err, "invalid_response")
	}
	c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.WorkerPauseHistoryEntry]{Items: []client.WorkerPauseHistoryEntry{row, row}}))
	_, err := c.WorkerPauseHistory(t.Context(), client.PageOptions{})
	checkError(t, err, "invalid_response")
	for _, body := range []string{`{"id":"worker-other","revision":1,"outcome":"updated"}`, `{"id":"worker-one","revision":0,"outcome":"updated"}`, `{"id":"worker-one","revision":1}`, `{"id":"worker-one","revision":null,"outcome":"updated"}`} {
		c, _ := progressResponseClient(t, body)
		_, err := c.ReplaceWorkerPause(t.Context(), "explicit", client.ReplaceWorkerPauseRequest{Paused: true})
		var e *client.Error
		if !errors.As(err, &e) || !e.OutcomeUnknown {
			t.Fatal("invalid receipt lost uncertainty", err)
		}
	}
}

func TestWorkerPauseClientCancellationDoesNotRetry(t *testing.T) {
	var reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		reads.Add(1)
		<-r.Context().Done()
	}))
	defer service.Close()
	c := newClient(t, service)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err := c.WorkerPause(ctx)
	checkError(t, err, "deadline_exceeded")
	if reads.Load() != 1 {
		t.Fatal("cancelled read retried", reads.Load())
	}
}
