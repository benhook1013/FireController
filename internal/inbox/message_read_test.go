package inbox_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestMessageReadPreservesFullBodyAndReceiptState(t *testing.T) {
	f := setup(t)
	root := f.send(f.a, f.clientA, f.sender, f.recipient, "root", "", "")
	body := "\n  ## Selected reply\n" + strings.Repeat("é", 7000) + "\n  "
	reply := f.send(f.a, f.clientA, f.recipient, f.sender, body, root.ID, "")
	events := f.eventCount(f.a)
	for _, worker := range []string{"", f.sender, f.recipient} {
		// Match the ordinary full conversation message shape, without receipt fields.
		var fields map[string]any
		w := f.request("GET", f.path(f.a, "messages/"+reply.ID), f.readonly, worker, "", nil)
		decode(t, w, http.StatusOK, &fields)
		if len(fields) != 7 || fields["id"] != reply.ID || fields["conversation_id"] != root.ConversationID || fields["sender_worker_id"] != f.recipient || fields["recipient_worker_id"] != f.sender || fields["reply_to_message_id"] != root.ID || fields["body"] != body || fields["created_at"] == "" {
			t.Fatalf("incorrect selected message shape: keys=%d", len(fields))
		}
	}
	var seen, acknowledged bool
	var revision int
	if err := f.store.DB().QueryRow("SELECT seen,acknowledged,revision FROM inbox_recipient_state WHERE project_id=? AND message_id=?", f.a, reply.ID).Scan(&seen, &acknowledged, &revision); err != nil {
		t.Fatal(err)
	}
	if seen || acknowledged || revision != 1 || f.eventCount(f.a) != events || f.providerCalls != 2 {
		t.Fatal("read changed receipt state, events or provider work")
	}
	var rootFields map[string]any
	decode(t, f.request("GET", f.path(f.a, "messages/"+root.ID), f.readonly, "", "", nil), http.StatusOK, &rootFields)
	if _, ok := rootFields["reply_to_message_id"]; ok || len(rootFields) != 6 {
		t.Fatal("root message must retain existing optional reply field semantics")
	}
}

func TestMessageReadScopesAndAdmission(t *testing.T) {
	f := setup(t)
	sent := f.send(f.a, f.clientA, f.sender, f.recipient, "selected", "", "")
	path := f.path(f.a, "messages/"+sent.ID)
	errorCode(t, f.request("GET", path, "", "", "", nil), http.StatusUnauthorized, "unauthorized")
	errorCode(t, f.request("GET", path, f.clientB, "", "", nil), http.StatusForbidden, "forbidden")
	errorCode(t, f.request("GET", f.path(f.b, "messages/"+sent.ID), f.clientB, "", "", nil), http.StatusNotFound, "not_found")
	errorCode(t, f.request("GET", f.path(f.a, "messages/missing"), f.readonly, "", "", nil), http.StatusNotFound, "not_found")
	errorCode(t, f.request("GET", path+"?limit=1", f.readonly, "", "", nil), http.StatusBadRequest, "validation_failed")
	errorCode(t, f.request("GET", path+"?cursor=unused", f.readonly, "", "", nil), http.StatusBadRequest, "validation_failed")
	decode(t, f.request("DELETE", f.path(f.a, "credentials/"+f.credentialA), f.admin, "", "revoke-message-reader", nil), http.StatusOK, nil)
	errorCode(t, f.request("GET", path, f.clientA, "", "", nil), http.StatusUnauthorized, "unauthorized")
}
