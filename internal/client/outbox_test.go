package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestOutboxAndDeliveryUseBoundReadsAndObserveRecipientHandling(t *testing.T) {
	f := newMutationFixture(t)
	base := "/api/v1/projects/" + f.project
	var paths []string
	var pathsMu sync.Mutex
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instance" {
			if r.Method != http.MethodGet || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != f.sender {
				t.Error("outgoing read lost binding or changed state")
			}
			pathsMu.Lock()
			paths = append(paths, r.URL.Path)
			pathsMu.Unlock()
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	first := f.call(t, http.MethodPost, base+"/messages", f.admin, f.sender, map[string]string{"recipient_worker_id": f.recipient, "body": strings.Repeat("é", 161)})["id"].(string)
	second := f.call(t, http.MethodPost, base+"/messages", f.admin, f.sender, map[string]string{"recipient_worker_id": f.recipient, "body": "seen request"})["id"].(string)
	f.call(t, http.MethodPost, base+"/messages/"+second+"/seen", f.admin, f.recipient, struct{}{})
	f.call(t, http.MethodPost, base+"/messages", f.admin, f.recipient, map[string]string{"recipient_worker_id": f.sender, "body": "incoming"})
	events := f.eventCount(t)
	page, err := c.Outbox(context.Background(), client.PageOptions{Limit: 1}, client.InboxFilter{Newest: true, Unacknowledged: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != second || !page.Items[0].Seen || page.Items[0].Acknowledged || !page.HasMore {
		t.Fatal(page, err)
	}
	next, err := c.Outbox(context.Background(), client.PageOptions{Limit: 1, Cursor: page.Cursor}, client.InboxFilter{Newest: true, Unacknowledged: true})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != first || !next.Items[0].PreviewTruncated || next.Items[0].Preview != strings.Repeat("é", 160) || next.HasMore {
		t.Fatal(next, err)
	}
	value, err := c.MessageDelivery(context.Background(), first)
	if err != nil || value.ID != first || value.SenderWorkerID != f.sender || value.RecipientWorkerID != f.recipient || value.Seen || value.Acknowledged {
		t.Fatal(value, err)
	}
	pathsMu.Lock()
	observedPaths := append([]string(nil), paths...)
	pathsMu.Unlock()
	if len(observedPaths) != 3 || observedPaths[0] != base+"/outbox/"+f.sender || observedPaths[1] != observedPaths[0] || observedPaths[2] != base+"/messages/"+first+"/delivery" || f.eventCount(t) != events || f.providerCalls.Load() != 1 {
		t.Fatal("read fanout or durable/provider side effect", observedPaths)
	}
	f.call(t, http.MethodPost, base+"/messages/"+first+"/ack", f.admin, f.recipient, struct{}{})
	value, err = c.MessageDelivery(context.Background(), first)
	if err != nil || !value.Seen || !value.Acknowledged {
		t.Fatal(value, err)
	}
	unread, err := c.Outbox(context.Background(), client.PageOptions{}, client.InboxFilter{Unread: true})
	if err != nil || len(unread.Items) != 0 || unread.HasMore {
		t.Fatal(unread, err)
	}
	// A new client discovers the same durable outbound history without a local receipt cache.
	reconnected := f.newClient(t, f.admin, f.sender, service.URL)
	all, err := reconnected.Outbox(context.Background(), client.PageOptions{}, client.InboxFilter{})
	if err != nil || len(all.Items) != 2 || !all.Items[0].Acknowledged {
		t.Fatal(all, err)
	}
}

func TestOutgoingClientRejectsMissingNullAndMisScopedReceiptFields(t *testing.T) {
	validItem := func() map[string]any {
		return map[string]any{
			"id": "message-one", "conversation_id": "conversation-one", "sender_worker_id": "worker-one", "recipient_worker_id": "worker-two", "preview": "handoff", "preview_truncated": false, "seen": false, "acknowledged": false, "created_at": "2026-10-27T01:00:00Z",
		}
	}
	validDelivery := func() map[string]any {
		return map[string]any{"id": "message-one", "sender_worker_id": "worker-two", "recipient_worker_id": "worker-three", "seen": false, "acknowledged": false}
	}
	for _, kind := range []string{"outbox", "delivery"} {
		t.Run(kind, func(t *testing.T) {
			valid := validItem
			if kind == "delivery" {
				valid = validDelivery
			}
			cases := map[string]func(map[string]any){
				"invalid-id":         func(m map[string]any) { m["id"] = "bad/id" },
				"empty-sender":       func(m map[string]any) { m["sender_worker_id"] = "" },
				"empty-recipient":    func(m map[string]any) { m["recipient_worker_id"] = "" },
				"impossible-ack":     func(m map[string]any) { m["acknowledged"] = true },
				"extra-private-body": func(m map[string]any) { m["body"] = "not part of this read" },
			}
			for field := range valid() {
				cases["missing-"+field] = func(m map[string]any) { delete(m, field) }
				cases["null-"+field] = func(m map[string]any) { m[field] = nil }
			}
			if kind == "outbox" {
				cases["wrong-sender"] = func(m map[string]any) { m["sender_worker_id"] = "another-worker" }
				cases["bad-conversation"] = func(m map[string]any) { m["conversation_id"] = "bad/id" }
				cases["long-preview"] = func(m map[string]any) { m["preview"] = strings.Repeat("é", 161) }
				cases["impossible-truncation"] = func(m map[string]any) { m["preview_truncated"] = true }
				cases["bad-date"] = func(m map[string]any) { m["created_at"] = "not-a-date" }
			} else {
				cases["wrong-id"] = func(m map[string]any) { m["id"] = "another-message" }
			}
			for name, change := range cases {
				t.Run(name, func(t *testing.T) {
					body := valid()
					change(body)
					service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/instance" {
							handshake(w)
							return
						}
						want := "/api/v1/projects/project-one/messages/message-one/delivery"
						var response any = body
						if kind == "outbox" {
							want = "/api/v1/projects/project-one/outbox/worker-one"
							response = map[string]any{"items": []any{body}, "has_more": false}
						}
						if r.Method != http.MethodGet || r.URL.Path != want || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
							t.Error("wrong outgoing read binding")
						}
						write(w, http.StatusOK, response)
					}))
					defer service.Close()
					c := newClient(t, service)
					var err error
					if kind == "outbox" {
						_, err = c.Outbox(context.Background(), client.PageOptions{}, client.InboxFilter{})
					} else {
						_, err = c.MessageDelivery(context.Background(), "message-one")
					}
					checkError(t, err, "invalid_response")
				})
			}
		})
	}
}

func TestOutgoingClientValidatesArgumentsFiltersAndCompletePage(t *testing.T) {
	var calls atomic.Int64
	var response any = map[string]any{"items": []any{}, "has_more": false}
	var responseMu sync.Mutex
	setResponse := func(value any) { responseMu.Lock(); response = value; responseMu.Unlock() }
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		responseMu.Lock()
		defer responseMu.Unlock()
		write(w, http.StatusOK, response)
	}))
	defer service.Close()
	c := newClient(t, service)
	for _, options := range []client.PageOptions{{Limit: -1}, {Limit: 101}, {Cursor: strings.Repeat("x", 4097)}} {
		_, err := c.Outbox(context.Background(), options, client.InboxFilter{})
		checkError(t, err, "validation_failed")
	}
	for _, id := range []string{"", "bad/id"} {
		_, err := c.MessageDelivery(context.Background(), id)
		checkError(t, err, "validation_failed")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments contacted domain API")
	}
	for _, bad := range []map[string]any{
		{"has_more": false}, {"items": nil, "has_more": false}, {"items": []any{}, "has_more": true},
		{"items": []any{}, "has_more": false, "cursor": "unexpected"},
	} {
		setResponse(bad)
		_, err := c.Outbox(context.Background(), client.PageOptions{}, client.InboxFilter{})
		checkError(t, err, "invalid_response")
	}
	item := map[string]any{"id": "message-one", "conversation_id": "conversation-one", "sender_worker_id": "worker-one", "recipient_worker_id": "worker-two", "preview": "handoff", "preview_truncated": false, "seen": true, "acknowledged": true, "created_at": "2026-10-27T01:00:00Z"}
	setResponse(map[string]any{"items": []any{item}, "has_more": false})
	for _, filter := range []client.InboxFilter{{Unread: true}, {Unacknowledged: true}} {
		_, err := c.Outbox(context.Background(), client.PageOptions{}, filter)
		checkError(t, err, "invalid_response")
	}
	// Delivery is deliberately a project read, including messages between other workers.
	setResponse(map[string]any{"id": "message-one", "sender_worker_id": "worker-two", "recipient_worker_id": "worker-three", "seen": true, "acknowledged": true})
	value, err := c.MessageDelivery(context.Background(), "message-one")
	if err != nil || !value.Seen || !value.Acknowledged {
		t.Fatal(value, err)
	}
	data, _ := json.Marshal(value)
	if strings.Contains(string(data), "body") {
		t.Fatal("delivery expanded a body")
	}
}
