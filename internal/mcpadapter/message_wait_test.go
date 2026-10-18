package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMessageWaitToolsSDKDiscoveryQuietConcurrentDeliveryAndCancel(t *testing.T) {
	f := mutationFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	streamCtx, stopStreams := context.WithCancel(t.Context())
	server.RegisterMessageStream(mux, f.store, streamCtx)
	entered, released := make(chan struct{}, 4), make(chan struct{}, 4)
	var streams, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/stream") {
			streams.Add(1)
			entered <- struct{}{}
			defer func() { released <- struct{}{} }()
		}
		mux.ServeHTTP(w, r)
	}))
	defer service.Close()
	defer stopStreams()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.recipient}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": identity}, f.credential), "chosen")
	tools, err := allDiscoveredTools(t, session)
	if err != nil || len(tools.Tools) == 0 || tools.NextCursor != "" {
		t.Fatal("discovery", err)
	}
	reads, writesCount := 0, 0
	found := false
	for _, tool := range tools.Tools {
		if tool.Name != "wait_for_messages" {
			continue
		}
		found = true
		if tool.Annotations.ReadOnlyHint {
			reads++
		} else {
			writesCount++
		}

		var schema struct {
			Properties map[string]any
			Required   []string
			Additional bool `json:"additionalProperties"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) != 3 || len(schema.Required) != 1 || schema.Required[0] != "cursor" || schema.Additional || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatal("schema/annotations")
		}
	}
	if !found || reads != 1 || writesCount != 0 {
		t.Fatal("census", found, reads, writesCount)
	}
	var snapshot client.InboxSnapshot
	decodeResult(t, call(t, session, "inbox_snapshot", nil), &snapshot)
	type outcome struct {
		value *mcp.CallToolResult
		err   error
	}
	start := func(ctx context.Context, args map[string]any) <-chan outcome {
		done := make(chan outcome, 1)
		go func() {
			v, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_messages", Arguments: args})
			done <- outcome{v, e}
		}()
		return done
	}
	done := start(t.Context(), map[string]any{"cursor": snapshot.EventCursor})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stream not opened")
	}
	// The same official SDK stdio session remains available during a pending wait.
	concurrentCtx, cancelRead := context.WithTimeout(t.Context(), time.Second)
	concurrent, err := session.CallTool(concurrentCtx, &mcp.CallToolParams{Name: "workers_list", Arguments: map[string]any{"limit": 1}})
	cancelRead()
	if err != nil || concurrent.IsError {
		t.Fatal("concurrent read blocked", err)
	}
	select {
	case result := <-done:
		t.Fatal("empty readiness exposed", result.err)
	case <-time.After(80 * time.Millisecond):
	}
	message := apiRequest(t, f.handler, "POST", "/api/v1/projects/"+f.project+"/messages", f.admin, "quiet-message", f.sender, `{"recipient_worker_id":"`+f.recipient+`","body":"Private source body"}`, 201)
	var page client.MessageEventPage
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		decodeResult(t, result.value, &page)
		raw, _ := json.Marshal(result.value)
		if strings.Contains(string(raw), "Private source body") {
			t.Fatal("full body leaked")
		}
	case <-time.After(time.Second):
		t.Fatal("message not delivered")
	}
	if len(page.Items) != 1 || page.Items[0].ResourceID != message["id"] || page.Items[0].Type != "message.sent" || page.TimedOut {
		t.Fatal("message page", page)
	}
	<-released
	var before int
	if err = f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done = start(ctx, map[string]any{"cursor": page.Cursor, "wait_seconds": 50, "limit": 100})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second stream not opened")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SDK cancellation not returned")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("forwarded stream leaked")
	}
	// Cancellation is a read and leaves the session usable for an explicit next wait.
	var expired client.MessageEventPage
	decodeResult(t, call(t, session, "wait_for_messages", map[string]any{"cursor": page.Cursor, "wait_seconds": 1}), &expired)
	if !expired.TimedOut || expired.Items == nil || len(expired.Items) != 0 || expired.HasMore || expired.Cursor == "" {
		t.Fatal("timeout", expired)
	}
	<-entered
	<-released
	var after, seen, ack int
	if err = f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&after); err != nil || after != before {
		t.Fatal("durable wait effect", err)
	}
	if err = f.store.DB().QueryRow("SELECT seen,acknowledged FROM inbox_recipient_state WHERE message_id=?", message["id"]).Scan(&seen, &ack); err != nil || seen != 0 || ack != 0 {
		t.Fatal("automatic acknowledgement", err)
	}
	if streams.Load() != 3 || writes.Load() != 0 || f.providers.Load() != 1 {
		t.Fatal("hidden retry/write/provider", streams.Load(), writes.Load(), f.providers.Load())
	}
}

func TestMessageWaitToolsInvalidBeforeCredentialsOrHTTP(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": identity}, "missing-credential"), "alpha"), "")
	for _, raw := range []string{`{}`, `null`, `{"cursor":null}`, `{"cursor":""}`, `{"cursor":" x"}`, `{"cursor":"x\n"}`, `{"cursor":"\ud800"}`, `{"cursor":"x","cursor":"y"}`, `{"Cursor":"x"}`, `{"cursor":"` + strings.Repeat("é", 2048) + `x"}`, `{"cursor":"x","limit":0}`, `{"cursor":"x","limit":101}`, `{"cursor":"x","limit":null}`, `{"cursor":"x","limit":1.0}`, `{"cursor":"x","wait_seconds":0}`, `{"cursor":"x","wait_seconds":51}`, `{"cursor":"x","wait_seconds":60}`, `{"cursor":"x","wait_seconds":61}`, `{"cursor":"x","wait_seconds":null}`, `{"cursor":"x","wait_seconds":"1"}`, `{"cursor":"x","wait_seconds":1e0}`, `{"cursor":"x","wait_seconds":1,"wait_seconds":2}`, `{"cursor":"x","worker_id":"other"}`, `{"cursor":"x","unread":true}`} {
		diagnosticResult(t, call(t, session, "wait_for_messages", json.RawMessage(raw)), "invalid_arguments", false)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input contacted service")
	}
}
