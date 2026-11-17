package mcpadapter_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestControllerOriginSDKIncomingWaitReadAckAndCancellation(t *testing.T) {
	f := mutationFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	streamCtx, stop := context.WithCancel(t.Context())
	defer stop()
	server.RegisterMessageStream(mux, f.store, streamCtx)
	entered, exited := make(chan struct{}, 2), make(chan struct{}, 2)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && (strings.Contains(r.URL.Path, "/inbox/") || strings.HasSuffix(r.URL.Path, "/stream") || strings.Contains(r.URL.Path, "/messages/")) && r.Header.Get(core.InboxOriginHeader) != "controller-v1" {
			t.Error("read did not opt into Controller origins")
		}
		stream := strings.HasSuffix(r.URL.Path, "/stream")
		if stream {
			entered <- struct{}{}
			defer func() { exited <- struct{}{} }()
		}
		mux.ServeHTTP(w, r)
	}))
	defer s.Close()
	binding := profile.Context{Profile: "chosen", ServerURL: s.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.recipient}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, f.credential), "chosen")
	var snapshot client.InboxSnapshot
	decodeResult(t, call(t, session, "inbox_snapshot", nil), &snapshot)
	type result struct {
		value *mcp.CallToolResult
		err   error
	}
	done := make(chan result, 1)
	go func() {
		v, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"cursor": snapshot.EventCursor, "wait_seconds": 900}})
		done <- result{v, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not enter")
	}
	var id string
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		id, err = inbox.PublishControllerMessage(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "service-lifecycle-sync"}, "sdk-notice", f.recipient, "PR #2 merged")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var events client.MessageEventPage
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		decodeResult(t, r.value, &events)
	case <-time.After(2 * time.Second):
		t.Fatal("Controller notice did not wake SDK wait")
	}
	if len(events.Items) != 1 || events.Items[0].ResourceID != id || events.Items[0].Payload.OriginKind != "controller" {
		t.Fatal(events)
	}
	<-exited
	var message client.Message
	decodeResult(t, call(t, session, "inbox_message_read", map[string]any{"message_id": id}), &message)
	if message.OriginKind != "controller" || message.SenderWorkerID != "" {
		t.Fatal(message)
	}
	var ack client.MessageReceipt
	decodeResult(t, call(t, session, "inbox_ack", map[string]any{"message_id": id, "idempotency_key": "sdk-controller-ack"}), &ack)
	if ack.Outcome != "acknowledged" {
		t.Fatal(ack)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		v, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: map[string]any{"cursor": events.Cursor, "wait_seconds": 900}})
		done <- result{v, err}
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SDK cancellation did not return")
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP wait retained after cancellation")
	}
}
