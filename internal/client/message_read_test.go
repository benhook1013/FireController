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

func TestMessageReadsSelectedReplyInOneRequestWithoutThread(t *testing.T) {
	f := newMutationFixture(t)
	path := "/api/v1/projects/" + f.project + "/messages"
	var rootID, lastID string
	body := "\n  ## Last reply\n" + strings.Repeat("é", 7000) + "\n  "
	for i := 0; i < 41; i++ {
		text := "older conversation context"
		if i == 40 {
			text = body
		}
		sent := f.call(t, http.MethodPost, path, f.admin, f.sender, map[string]string{"recipient_worker_id": f.recipient, "body": text, "reply_to_message_id": lastID})
		lastID = sent["id"].(string)
		if i == 0 {
			rootID = lastID
		}
	}
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instance" {
			calls.Add(1)
			if r.Method != "GET" || r.URL.Path != path+"/"+lastID || r.URL.RawQuery != "" || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != f.recipient {
				t.Error("read was not a single selected bound message GET")
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	c := f.newClient(t, f.admin, f.recipient, service.URL)
	events := f.eventCount(t)
	value, err := c.Message(context.Background(), lastID)
	if err != nil || value.ID != lastID || value.Body != body || value.SenderWorkerID != f.sender || value.RecipientWorkerID != f.recipient || value.ReplyToMessageID == "" || value.ReplyToMessageID == rootID || calls.Load() != 1 {
		t.Fatalf("selected last message failed: calls=%d err=%v", calls.Load(), err)
	}
	var changed int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM inbox_recipient_state WHERE seen != 0 OR acknowledged != 0 OR revision != 1").Scan(&changed); err != nil {
		t.Fatal(err)
	}
	if changed != 0 || f.eventCount(t) != events || f.providerCalls.Load() != 1 {
		t.Fatal("selected read changed receipts, events or provider work")
	}
}

func TestMessageRejectsInvalidInputsBeforeTransport(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handshake(w)
	}))
	defer service.Close()
	c := newClient(t, service)
	before := calls.Load()
	for _, id := range []string{"", "../other", "a/b", " padded", "a?project=other", strings.Repeat("a", 129)} {
		_, err := c.Message(context.Background(), id)
		checkError(t, err, "validation_failed")
	}
	if calls.Load() != before {
		t.Fatal("invalid message ID reached transport")
	}
}

func TestMessageRejectsInvalidResponsesAndRetainsBinding(t *testing.T) {
	valid := func() client.Message {
		return client.Message{ID: "message-one", ConversationID: "conversation-one", SenderWorkerID: "sender-one", RecipientWorkerID: "recipient-one", Body: "  full body\n", CreatedAt: "2026-10-22T00:00:00Z"}
	}
	for name, change := range map[string]func(*client.Message){
		"wrong-message":        func(m *client.Message) { m.ID = "message-other" },
		"invalid-conversation": func(m *client.Message) { m.ConversationID = "../other" },
		"missing-sender":       func(m *client.Message) { m.SenderWorkerID = "" },
		"missing-recipient":    func(m *client.Message) { m.RecipientWorkerID = "" },
		"invalid-reply":        func(m *client.Message) { m.ReplyToMessageID = "a/b" },
		"self-reply":           func(m *client.Message) { m.ReplyToMessageID = m.ID },
		"blank-body":           func(m *client.Message) { m.Body = " \n\t" },
		"oversized-body":       func(m *client.Message) { m.Body = strings.Repeat("x", (16<<10)+1) },
		"bad-timestamp":        func(m *client.Message) { m.CreatedAt = "not-a-timestamp" },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid()
			change(&value)
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				if r.URL.Path != "/api/v1/projects/project-one/messages/message-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("selected message request lost project, instance or credential binding")
				}
				write(w, 200, value)
			}))
			defer service.Close()
			_, err := newClient(t, service).Message(context.Background(), "message-one")
			checkError(t, err, "invalid_response")
		})
	}
}
