package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Observe the actual server's first flushed SSE frame without changing its
// response deadlines, body or subscription lifecycle.
type concurrentMessageWaitReadyWriter struct {
	http.ResponseWriter
	ready chan<- struct{}
	once  sync.Once
}

func (w *concurrentMessageWaitReadyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *concurrentMessageWaitReadyWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	w.once.Do(func() { w.ready <- struct{}{} })
}

func TestConcurrentMessageWaitDoesNotBlockNativeMutations(t *testing.T) {
	f := mutationFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	streamCtx, stopStreams := context.WithCancel(t.Context())
	server.RegisterMessageStream(mux, f.store, streamCtx)
	ready, detached := make(chan struct{}, 1), make(chan struct{}, 1)
	var active, streams, sends, updates, creates atomic.Int64
	var jobID string
	messageBody := "Unrelated outgoing message during an incoming wait"
	originalBrief, changedBrief := "Original unrelated brief", "Deliberately updated unrelated brief"
	expectedSend, _ := json.Marshal(client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: messageBody})
	expectedUpdate, _ := json.Marshal(client.UpdateJobRequest{ExpectedRevision: 1, Brief: &changedBrief})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/message-events/stream") {
			if r.Method != "GET" || r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Idempotency-Key") != "" {
				t.Error("SSE binding/action changed")
			}
			streams.Add(1)
			active.Add(1)
			defer func() { active.Add(-1); detached <- struct{}{} }()
			mux.ServeHTTP(&concurrentMessageWaitReadyWriter{ResponseWriter: w, ready: ready}, r)
			return
		}
		if r.Method != "GET" {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
				t.Error("mutation binding changed")
			}
			switch {
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
				sends.Add(1)
				if r.Header.Get("Idempotency-Key") != "concurrent-sse-message" || !bytes.Equal(raw, expectedSend) {
					t.Error("message request/key changed")
				}
			case r.Method == "PATCH" && r.URL.Path == "/api/v1/projects/"+f.project+"/jobs/"+jobID:
				updates.Add(1)
				if r.Header.Get("Idempotency-Key") != "concurrent-sse-job-update" || !bytes.Equal(raw, expectedUpdate) {
					t.Error("job request/key changed")
				}
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/jobs"):
				creates.Add(1)
			default:
				t.Error("unexpected mutation")
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(service.Close)
	t.Cleanup(stopStreams)
	// Own both stdio session ends so cancelled replies drain before pipe closure.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	lifetime, cancelLifetime := context.WithTimeout(t.Context(), 15*time.Second)
	binding := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	ss, err := adapter(t, profiles(t, map[string]profile.Context{"chosen": binding}, f.credential), "chosen").Connect(lifetime, &mcp.IOTransport{Reader: inR, Writer: outW}, nil)
	if err != nil {
		cancelLifetime()
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "concurrent-inbox-stdio-fixture", Version: "1"}, nil).Connect(lifetime, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		ss.Close()
		cancelLifetime()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Error("server close", err)
		}
		if err := cs.Close(); err != nil {
			t.Error("client close", err)
		}
		cancelLifetime()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
	})
	var created client.JobReceipt
	decodeResult(t, call(t, cs, "jobs_create", map[string]any{"title": "Unrelated recipient job", "brief": originalBrief, "worker_id": f.recipient, "idempotency_key": "concurrent-sse-job-create"}), &created)
	jobID = created.ID
	var snapshot client.InboxSnapshot
	decodeResult(t, call(t, cs, "inbox_snapshot", nil), &snapshot)
	waitCtx, cancelWait := context.WithCancel(t.Context())
	done := make(chan struct{}, 1)
	joined := false
	join := func() {
		if joined {
			return
		}
		joined = true
		cancelWait()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			t.Error("SDK incoming wait did not join")
			return
		}
		select {
		case <-detached:
		case <-timer.C:
			t.Error("actual server SSE did not detach")
		}
	}
	t.Cleanup(join)
	go func() {
		_, _ = cs.CallTool(waitCtx, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"cursor": snapshot.EventCursor, "wait_seconds": 45}})
		done <- struct{}{}
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
	case <-timer.C:
		t.Fatal("actual server SSE readiness not flushed")
	}
	assertPending := func() {
		t.Helper()
		if active.Load() != 1 {
			t.Fatal("SSE ended before concurrent operations finished")
		}
		select {
		case <-done:
			t.Fatal("incoming wait completed on unrelated work")
		default:
		}
	}
	assertPending()
	invoke := func(name string, args any) *mcp.CallToolResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("concurrent %s blocked: %v", name, err)
		}
		assertPending()
		return result
	}
	send := map[string]any{"recipient_worker_id": f.recipient, "body": messageBody, "idempotency_key": "concurrent-sse-message"}
	update := map[string]any{"job_id": jobID, "expected_revision": 1, "brief": changedBrief, "idempotency_key": "concurrent-sse-job-update"}
	var message, messageReplay client.MessageReceipt
	var edited, editReplay client.JobReceipt
	decodeResult(t, invoke("inbox_send", send), &message)
	decodeResult(t, invoke("job_update", update), &edited)
	decodeResult(t, invoke("inbox_send", send), &messageReplay)
	decodeResult(t, invoke("job_update", update), &editReplay)
	if message.ID == "" || message != messageReplay || edited.ID != jobID || edited.Revision != 2 || edited != editReplay {
		t.Fatal("mutation receipts/replay changed")
	}
	var history client.Page[client.JobHistoryEntry]
	decodeResult(t, invoke("job_history", map[string]any{"job_id": jobID}), &history)
	if history.HasMore || len(history.Items) != 2 || history.Items[0].State.Brief != originalBrief || history.Items[1].State.Brief != changedBrief {
		t.Fatal("job history did not preserve two deliberate revisions")
	}
	var count, seen, ack int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM inbox_messages WHERE project_id=? AND sender_worker_id=? AND recipient_worker_id=? AND body=?`, f.project, f.sender, f.recipient, messageBody).Scan(&count); err != nil || count != 1 {
		t.Fatal("message effect not exactly once", count, err)
	}
	if err := f.store.DB().QueryRow(`SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?`, message.ID).Scan(&seen, &ack); err != nil || seen != 0 || ack != 0 {
		t.Fatal("wait changed another recipient state", err)
	}
	var eventsBefore, eventsAfter int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	join()
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&eventsAfter); err != nil || eventsAfter != eventsBefore {
		t.Fatal("wait cancellation changed durable events", err)
	}
	if active.Load() != 0 || streams.Load() != 1 || sends.Load() != 2 || updates.Load() != 2 || creates.Load() != 1 || f.providers.Load() != 1 {
		t.Fatal("hidden stream retry/mutation/provider work or unjoined SSE")
	}
	t.Log("same stdio SDK session: actual server SSE pending at wait45 while message send/job update and exact-key replays finish; one message/two job revisions; cancellation detached and joined")
}
