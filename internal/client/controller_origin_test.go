package client_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/server"
)

func TestControllerOriginClientActualReadsEventsAndOneWayReply(t *testing.T) {
	f := newMutationFixture(t)
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	server.RegisterMessageStream(mux, f.store, t.Context())
	s := httptest.NewServer(mux)
	defer s.Close()
	c := f.newClient(t, f.admin, f.recipient, s.URL)
	snapshot, err := c.InboxSnapshot(t.Context(), client.PageOptions{}, client.InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		id, err = inbox.PublishControllerMessage(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "service-lifecycle-sync"}, "client-notice", f.recipient, "PR #2 merged")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := f.call(t, http.MethodPost, "/api/v1/projects/"+f.project+"/messages", f.admin, f.sender, map[string]string{"recipient_worker_id": f.recipient, "body": "Worker message"})
	// Headerless older clients filter Controller before the page bound.
	r := httptest.NewRequest("GET", "/api/v1/projects/"+f.project+"/inbox/"+f.recipient+"?limit=1", nil)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", f.recipient)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var old client.Page[client.InboxItem]
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &old) != nil || len(old.Items) != 1 || old.Items[0].ID != worker["id"] || strings.Contains(w.Body.String(), "origin_kind") {
		t.Fatal(w.Code, w.Body.String())
	}
	page, err := c.Inbox(t.Context(), client.PageOptions{Limit: 1}, client.InboxFilter{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != id || page.Items[0].OriginKind != "controller" || page.Items[0].SenderWorkerID != "" || !page.HasMore {
		t.Fatal(page, err)
	}
	message, err := c.Message(t.Context(), id)
	if err != nil || message.OriginKind != "controller" {
		t.Fatal(message, err)
	}
	conversation, err := c.Conversation(t.Context(), message.ConversationID, client.PageOptions{})
	if err != nil || len(conversation.Items) != 1 || conversation.HasMore {
		t.Fatal(conversation, err)
	}
	events, err := c.MessageEvents(t.Context(), snapshot.EventCursor, 1, 0)
	if err != nil || len(events.Items) != 1 || events.Items[0].ResourceID != id || events.Items[0].Payload.OriginKind != "controller" || events.Items[0].Payload.SenderWorkerID == nil || *events.Items[0].Payload.SenderWorkerID != "" {
		t.Fatal(events, err)
	}
	waited, err := c.WaitForMessages(t.Context(), snapshot.EventCursor, 1, 1)
	if err != nil || len(waited.Items) != 1 || waited.Items[0].ResourceID != id {
		t.Fatal(waited, err)
	}
	_, err = c.SendMessage(t.Context(), "cannot-reply", client.SendMessageRequest{RecipientWorkerID: f.sender, Body: "Follow-up", ReplyToMessageID: id})
	checkError(t, err, "controller_message_one_way")
	_, err = c.Acknowledge(t.Context(), "ack-notice", id)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := c.MessageDelivery(t.Context(), id)
	if err != nil || delivery.OriginKind != "controller" || !delivery.Seen || !delivery.Acknowledged {
		t.Fatal(delivery, err)
	}
	if f.providerCalls.Load() != 1 {
		t.Fatal("inbox performed provider work")
	}
}

func TestControllerOriginClientRejectsIncompleteOrForgedUnion(t *testing.T) {
	base := `{"id":"message-one","conversation_id":"conversation-one","sender_worker_id":"","recipient_worker_id":"recipient-one","body":"notice","created_at":"2026-11-02T00:00:00Z"}`
	for name, field := range map[string]string{"missing": "", "null": `,"origin_kind":null`, "empty": `,"origin_kind":""`, "other": `,"origin_kind":"other"`, "worker": `,"origin_kind":"worker"`, "reply": `,"origin_kind":"controller","reply_to_message_id":"other"`, "impersonated": `,"origin_kind":"controller"`} {
		t.Run(name, func(t *testing.T) {
			raw := strings.TrimSuffix(base, "}") + field + "}"
			if name == "impersonated" {
				raw = strings.Replace(raw, `"sender_worker_id":""`, `"sender_worker_id":"worker-one"`, 1)
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(raw))
			}))
			defer s.Close()
			_, err := newClient(t, s).Message(t.Context(), "message-one")
			checkError(t, err, "invalid_response")
		})
	}
}
