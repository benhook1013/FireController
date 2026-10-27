package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func validMessageEventPageJSON() string {
	return `{"items":[{"sequence":7,"project_id":"project-one","type":"message.sent","resource_id":"message-one","revision":1,"actor_id":"principal-one","payload":{"conversation_id":"conversation-one","recipient_worker_id":"worker-one","sender_worker_id":"sender-one"},"created_at":"2026-10-15T12:30:00Z"}],"cursor":"signed-next","has_more":false,"timed_out":false}`
}

func TestMessageEventClientUsesBoundRoutesAndTypedPayloads(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("event request was not bound to the verified instance and selected worker")
		}
		switch r.URL.Path {
		case "/api/v1/projects/project-one/inbox/worker-one/snapshot":
			if r.URL.Query().Get("unread") != "true" || r.URL.Query().Get("unacknowledged") != "true" || r.URL.Query().Get("limit") != "1" {
				t.Errorf("snapshot query mismatch: %s", r.URL.RawQuery)
			}
			write(w, 200, map[string]any{"inbox": client.Page[client.InboxItem]{Items: []client.InboxItem{{ID: "message-one", ConversationID: "conversation-one", SenderWorkerID: "sender-one", RecipientWorkerID: "worker-one", Preview: "preview", CreatedAt: "2026-10-15T12:30:00Z"}}, HasMore: true, Cursor: "inbox-next"}, "event_cursor": "signed-snapshot"})
		case "/api/v1/projects/project-one/message-events":
			if r.URL.Query().Get("include_assignments") != "true" || r.URL.Query().Get("cursor") != "signed-snapshot" || r.URL.Query().Get("limit") != "1" && r.URL.Query().Get("limit") != "" || r.URL.Query().Get("wait_ms") != "275" && r.URL.Query().Get("wait_ms") != "" {
				t.Errorf("event query mismatch: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validMessageEventPageJSON()))
		default:
			t.Errorf("unexpected event endpoint %s", r.URL.Path)
			write(w, 404, map[string]string{"error": "missing"})
		}
	}))
	defer server.Close()
	c := newClient(t, server)
	snapshot, err := c.InboxSnapshot(context.Background(), client.PageOptions{Limit: 1}, client.InboxFilter{Unread: true, Unacknowledged: true})
	if err != nil || snapshot.EventCursor != "signed-snapshot" || len(snapshot.Inbox.Items) != 1 || snapshot.Inbox.Cursor != "inbox-next" || !snapshot.Inbox.HasMore {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	page, err := c.MessageEvents(context.Background(), snapshot.EventCursor, 1, 275)
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 7 || page.Items[0].Payload.SenderWorkerID == nil || *page.Items[0].Payload.SenderWorkerID != "sender-one" || page.TimedOut {
		t.Fatalf("event page=%+v err=%v", page, err)
	}
	if _, err := c.MessageEvents(context.Background(), snapshot.EventCursor, 0, 0); err != nil {
		t.Fatalf("server-default event page: %v", err)
	}
	before := calls.Load()
	for _, tc := range []struct {
		cursor string
		limit  int
		waitMS int
	}{
		{cursor: ""}, {cursor: "cursor", limit: -1}, {cursor: "cursor", limit: 101}, {cursor: "cursor", waitMS: -1}, {cursor: "cursor", waitMS: 4001},
	} {
		if _, err := c.MessageEvents(context.Background(), tc.cursor, tc.limit, tc.waitMS); err == nil {
			t.Fatalf("invalid event arguments accepted: %+v", tc)
		}
	}
	if _, err := c.InboxSnapshot(context.Background(), client.PageOptions{Limit: -1}, client.InboxFilter{}); err == nil {
		t.Fatal("invalid snapshot page options accepted")
	}
	if calls.Load() != before {
		t.Fatal("invalid event arguments made a service request")
	}
}

func TestMessageEventClientRejectsMalformedOrUnscopedPages(t *testing.T) {
	valid := validMessageEventPageJSON()
	bodies := []struct {
		name, body string
	}{
		{"missing_items", `{"cursor":"next","has_more":false,"timed_out":false}`},
		{"null_items", `{"items":null,"cursor":"next","has_more":false,"timed_out":false}`},
		{"missing_cursor", `{"items":[],"has_more":false,"timed_out":false}`},
		{"empty_cursor", `{"items":[],"cursor":"","has_more":false,"timed_out":false}`},
		{"wrong_project", strings.Replace(valid, `"project-one"`, `"other-project"`, 1)},
		{"wrong_worker", strings.Replace(valid, `"worker-one"`, `"other-worker"`, 1)},
		{"missing_sender", strings.Replace(valid, `,"sender_worker_id":"sender-one"`, "", 1)},
		{"sender_on_seen", strings.Replace(valid, `"message.sent"`, `"message.seen"`, 1)},
		{"wrong_type", strings.Replace(valid, `"message.sent"`, `"worker.created"`, 1)},
		{"body_in_payload", strings.Replace(valid, `"sender_worker_id":"sender-one"`, `"sender_worker_id":"sender-one","body":"private"`, 1)},
		{"bad_timestamp", strings.Replace(valid, `2026-10-15T12:30:00Z`, `not-a-time`, 1)},
		{"zero_sequence", strings.Replace(valid, `"sequence":7`, `"sequence":0`, 1)},
		{"inconsistent_timeout", strings.Replace(valid, `"timed_out":false`, `"timed_out":true`, 1)},
		{"duplicate_field", strings.Replace(valid, `"sequence":7`, `"sequence":7,"sequence":8`, 1)},
	}
	for _, tc := range bodies {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			c := newClient(t, server)
			_, err := c.MessageEvents(context.Background(), "cursor", 10, 0)
			checkError(t, err, "invalid_response")
			c.Close()
			server.Close()
		})
	}
}

func TestInboxSnapshotClientRejectsMalformedPageAndCursor(t *testing.T) {
	for _, body := range []string{
		`{"inbox":{"items":[],"cursor":"","has_more":false},"event_cursor":""}`,
		`{"inbox":{"items":null,"cursor":"","has_more":false},"event_cursor":"signed"}`,
		`{"inbox":{"items":[{"id":"message-one","conversation_id":"conversation-one","sender_worker_id":"sender-one","recipient_worker_id":"other-worker","preview":"x","preview_truncated":false,"seen":false,"acknowledged":false,"created_at":"2026-10-15T12:30:00Z"}],"cursor":"","has_more":false},"event_cursor":"signed"}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		c := newClient(t, server)
		_, err := c.InboxSnapshot(context.Background(), client.PageOptions{}, client.InboxFilter{})
		checkError(t, err, "invalid_response")
		c.Close()
		server.Close()
	}
}
