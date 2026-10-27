package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/server"
)

type assignmentReadyWriter struct {
	http.ResponseWriter
	once  sync.Once
	ready chan struct{}
}

func (w *assignmentReadyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *assignmentReadyWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	w.once.Do(func() { w.ready <- struct{}{} })
}

func TestAssignmentEventsActualQuietWaitFiniteReplayReassignmentAndCancellation(t *testing.T) {
	f := newMutationFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	server.RegisterMessageStream(mux, f.store, t.Context())
	ready := make(chan struct{}, 4)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/message-events/stream") {
			if r.URL.Query().Get("include_assignments") != "true" {
				t.Error("typed wait did not select assignments")
			}
			mux.ServeHTTP(&assignmentReadyWriter{ResponseWriter: w, ready: ready}, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	receiver, sender := f.newClient(t, f.admin, f.recipient, s.URL), f.newClient(t, f.admin, f.sender, s.URL)
	snapshot, err := receiver.InboxSnapshot(t.Context(), client.PageOptions{}, client.InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		page client.MessageEventPage
		err  error
	}
	finished := make(chan result, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	go func() { p, e := receiver.WaitForMessages(ctx, snapshot.EventCursor, 20, 3); finished <- result{p, e} }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("quiet wait never established")
	}
	// Self assignment and unrelated ownership must not complete this wait.
	if _, err := sender.CreateJob(t.Context(), "self-create", client.CreateJobRequest{Title: "Own work", WorkerID: f.sender}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		t.Fatal("irrelevant or self assignment completed another worker's wait", result)
	case <-time.After(30 * time.Millisecond):
	}
	created, err := sender.CreateJob(t.Context(), "incoming-create", client.CreateJobRequest{Title: "PRIVATE_TITLE", Brief: "PRIVATE_BRIEF", WorkerID: f.recipient})
	if err != nil {
		t.Fatal(err)
	}
	var delivered client.MessageEventPage
	select {
	case result := <-finished:
		if result.err != nil || len(result.page.Items) != 1 || result.page.Items[0].Type != "job.assigned" || result.page.Items[0].ResourceID != created.ID || result.page.TimedOut {
			t.Fatal("assignment did not complete the original quiet wait", result)
		}
		delivered = result.page
	case <-ctx.Done():
		t.Fatal("committed assignment did not wake the active quiet wait")
	}
	replay, err := receiver.MessageEvents(t.Context(), snapshot.EventCursor, 20, 0)
	if err != nil || len(replay.Items) != 1 || replay.Items[0].Sequence != delivered.Items[0].Sequence {
		t.Fatal("finite replay did not retain quiet assignment delivery", replay, err)
	}
	legacy := f.call(t, "GET", "/api/v1/projects/"+f.project+"/message-events?cursor="+url.QueryEscape(snapshot.EventCursor), f.admin, f.recipient, nil)
	if len(legacy["items"].([]any)) != 0 {
		t.Fatal("old HTTP request unexpectedly received assignment events", legacy)
	}
	advanced, err := receiver.MessageEvents(t.Context(), legacy["cursor"].(string), 20, 0)
	if err != nil || len(advanced.Items) != 0 {
		t.Fatal("new selection retrospectively replayed a filtered suffix", advanced, err)
	}
	senderSnapshot, err := sender.InboxSnapshot(t.Context(), client.PageOptions{}, client.InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	f.call(t, "PATCH", "/api/v1/projects/"+f.project+"/jobs/"+created.ID, f.admin, "", map[string]any{"expected_revision": 1, "worker_id": f.sender})
	for _, target := range []struct {
		c      *client.Client
		cursor string
		kind   string
	}{{receiver, delivered.Cursor, "job.unassigned"}, {sender, senderSnapshot.EventCursor, "job.assigned"}} {
		page, err := target.c.MessageEvents(t.Context(), target.cursor, 20, 0)
		if err != nil || len(page.Items) != 1 || page.Items[0].Type != target.kind || page.Items[0].Revision != 2 || page.Items[0].Payload.SenderWorkerID != nil || page.Items[0].Payload.ConversationID != "" {
			t.Fatal("administrative transfer lost affected-worker attention", page, err)
		}
	}
	before, providerCalls := f.eventCount(t), f.providerCalls.Load()
	ctxCancel, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { p, e := receiver.WaitForMessages(ctxCancel, advanced.Cursor, 20, 3); finished <- result{p, e} }()
	// This cursor precedes unassignment, so consume that attention first.
	first := <-finished
	if first.err != nil || len(first.page.Items) != 1 || first.page.Items[0].Type != "job.unassigned" {
		t.Fatal("assignment readiness page did not return relevant backlog", first)
	}
	<-ready
	go func() {
		p, e := receiver.WaitForMessages(ctxCancel, first.page.Cursor, 20, 3)
		finished <- result{p, e}
	}()
	<-ready
	stop()
	select {
	case result := <-finished:
		checkError(t, result.err, "canceled")
	case <-time.After(time.Second):
		t.Fatal("cancelled assignment wait retained its connection")
	}
	if f.eventCount(t) != before || f.providerCalls.Load() != providerCalls {
		t.Fatal("attention reads or cancellation changed state or called a provider")
	}
	inbox, err := receiver.Inbox(t.Context(), client.PageOptions{}, client.InboxFilter{})
	if err != nil || len(inbox.Items) != 0 {
		t.Fatal("assignments manufactured durable messages", inbox, err)
	}
}

func TestAssignmentEventClientStrictPerKindPayloads(t *testing.T) {
	valid := `{"items":[{"sequence":7,"project_id":"project-one","type":"job.assigned","resource_id":"job-one","revision":2,"actor_id":"principal-one","payload":{"recipient_worker_id":"worker-one"},"created_at":"2026-10-15T12:30:00Z"}],"cursor":"next","has_more":false,"timed_out":false}`
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"admin", valid, true},
		{"worker", strings.Replace(valid, `"recipient_worker_id":"worker-one"`, `"recipient_worker_id":"worker-one","sender_worker_id":"other-worker"`, 1), true},
		{"unassigned", strings.Replace(valid, "job.assigned", "job.unassigned", 1), true},
		{"conversation", strings.Replace(valid, `"recipient_worker_id":"worker-one"`, `"recipient_worker_id":"worker-one","conversation_id":"conversation-one"`, 1), false},
		{"self", strings.Replace(valid, `"recipient_worker_id":"worker-one"`, `"recipient_worker_id":"worker-one","sender_worker_id":"worker-one"`, 1), false},
		{"bad_sender", strings.Replace(valid, `"recipient_worker_id":"worker-one"`, `"recipient_worker_id":"worker-one","sender_worker_id":""`, 1), false},
		{"message_without_conversation", strings.Replace(valid, "job.assigned", "message.acknowledged", 1), false},
		{"private_body", strings.Replace(valid, `"recipient_worker_id":"worker-one"`, `"recipient_worker_id":"worker-one","body":"PRIVATE"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := waitServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			})
			_, err := newClient(t, s).MessageEvents(t.Context(), "saved", 20, 0)
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			if !tc.valid {
				checkError(t, err, "invalid_response")
			}
		})
	}
	// Ordinary message wire bytes keep their conversation key.
	message := waitEvent(7, "message.sent")
	encoded, err := json.Marshal(message.Payload)
	if err != nil || !strings.Contains(string(encoded), `"conversation_id":"conversation-one"`) {
		t.Fatal("ordinary message metadata changed", string(encoded), err)
	}
}
