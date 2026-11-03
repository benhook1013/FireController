package inbox_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func outbox(t *testing.T, f *fixture, worker, query string) page {
	t.Helper()
	var result page
	decode(t, f.request(http.MethodGet, f.path(f.a, "outbox/"+worker)+query, f.readonly, "", "", nil), http.StatusOK, &result)
	return result
}

func delivery(t *testing.T, f *fixture, id string) map[string]any {
	t.Helper()
	var result map[string]any
	decode(t, f.request(http.MethodGet, f.path(f.a, "messages/"+id+"/delivery"), f.readonly, "", "", nil), http.StatusOK, &result)
	return result
}

func TestOutboxDiscoveryAndSelectedDeliveryKeepRecipientStateExplicit(t *testing.T) {
	f := setup(t)
	first := f.send(f.a, f.clientA, f.sender, f.recipient, strings.Repeat("é", 161), "", "")
	seen := f.send(f.a, f.clientA, f.sender, f.recipient, "seen but not handled", "", "")
	handled := f.send(f.a, f.clientA, f.sender, f.recipient, "handled", "", "")
	f.send(f.a, f.clientA, f.recipient, f.sender, "incoming reply", first.ID, "")
	f.send(f.b, f.clientB, f.bSender, f.bRecipient, "foreign outgoing", "", "")
	f.transition(f.a, f.clientA, f.recipient, seen.ID, "seen", "")
	f.transition(f.a, f.clientA, f.recipient, handled.ID, "ack", "")
	incoming := f.request(http.MethodGet, f.path(f.a, "inbox/"+f.sender), f.readonly, "", "", nil).Body.String()
	events := f.eventCount(f.a)
	all := outbox(t, f, f.sender, "")
	if len(all.Items) != 3 || all.HasMore || all.Items[0].ID != first.ID || all.Items[1].ID != seen.ID || all.Items[2].ID != handled.ID || all.Items[0].Preview != strings.Repeat("é", 160) || !all.Items[0].PreviewTruncated {
		t.Fatal("sender discovery lost original order or bounded preview", all)
	}
	for _, item := range all.Items {
		if item.SenderWorkerID != f.sender || item.RecipientWorkerID != f.recipient || item.ConversationID == "" || item.Body != "" {
			t.Fatal("outbox leaked full text or lost identities", item)
		}
	}
	if all.Items[0].Seen || all.Items[0].Acknowledged || !all.Items[1].Seen || all.Items[1].Acknowledged || !all.Items[2].Seen || !all.Items[2].Acknowledged {
		t.Fatal("sender view does not expose actual recipient state")
	}
	unread := outbox(t, f, f.sender, "?unread=true")
	pending := outbox(t, f, f.sender, "?unacknowledged=true&newest=true")
	if len(unread.Items) != 1 || unread.Items[0].ID != first.ID || len(pending.Items) != 2 || pending.Items[0].ID != seen.ID || pending.Items[1].ID != first.ID {
		t.Fatal("outgoing receipt filters select the wrong state")
	}
	for _, pair := range []struct {
		id                 string
		seen, acknowledged bool
	}{{first.ID, false, false}, {seen.ID, true, false}, {handled.ID, true, true}} {
		value := delivery(t, f, pair.id)
		if len(value) != 5 || value["id"] != pair.id || value["sender_worker_id"] != f.sender || value["recipient_worker_id"] != f.recipient || value["seen"] != pair.seen || value["acknowledged"] != pair.acknowledged {
			t.Fatal("selected receipt must be compact and complete", value)
		}
	}
	if after := f.request(http.MethodGet, f.path(f.a, "inbox/"+f.sender), f.readonly, "", "", nil).Body.String(); after != incoming || f.eventCount(f.a) != events || f.providerCalls != 2 {
		t.Fatal("outgoing reads changed incoming wire output, events or providers")
	}
	// Recipient writes remain explicit. A sender cannot acknowledge its handoff.
	errorCode(t, f.request(http.MethodPost, f.path(f.a, "messages/"+first.ID+"/ack"), f.clientA, f.sender, "sender-ack-refused", struct{}{}), http.StatusForbidden, "forbidden")
	f.transition(f.a, f.clientA, f.recipient, first.ID, "ack", "")
	if value := delivery(t, f, first.ID); value["seen"] != true || value["acknowledged"] != true {
		t.Fatal("selected delivery did not observe later recipient handling")
	}
	// Durable discovery survives restart without retaining the send receipt in a client.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if page := outbox(t, f, f.sender, ""); len(page.Items) != 3 || !page.Items[0].Acknowledged {
		t.Fatal("outgoing discovery or handling state was lost on reopen")
	}
}

func TestOutboxContinuationIsBoundToSenderDirectionFiltersAndPrincipal(t *testing.T) {
	f := setup(t)
	first := f.send(f.a, f.clientA, f.sender, f.recipient, "first", "", "")
	second := f.send(f.a, f.clientA, f.sender, f.recipient, "second", "", "")
	third := f.send(f.a, f.clientA, f.sender, f.recipient, "third", "", "")
	f.send(f.a, f.clientA, f.recipient, f.sender, "incoming one", "", "")
	f.send(f.a, f.clientA, f.recipient, f.sender, "incoming two", "", "")
	oldest := outbox(t, f, f.sender, "?limit=1")
	if len(oldest.Items) != 1 || oldest.Items[0].ID != first.ID || !oldest.HasMore {
		t.Fatal(oldest)
	}
	newest := outbox(t, f, f.sender, "?limit=1&newest=true&unacknowledged=true")
	if len(newest.Items) != 1 || newest.Items[0].ID != third.ID || !newest.HasMore {
		t.Fatal(newest)
	}
	f.send(f.a, f.clientA, f.sender, f.recipient, "arrived later", "", "")
	next := outbox(t, f, f.sender, "?limit=1&newest=true&unacknowledged=true&cursor="+url.QueryEscape(newest.Cursor))
	if len(next.Items) != 1 || next.Items[0].ID != second.ID || !next.HasMore {
		t.Fatal(next)
	}
	last := outbox(t, f, f.sender, "?limit=1&newest=true&unacknowledged=true&cursor="+url.QueryEscape(next.Cursor))
	if len(last.Items) != 1 || last.Items[0].ID != first.ID || last.HasMore || last.Cursor != "" {
		t.Fatal(last)
	}
	incoming := f.inbox(f.a, f.readonly, f.sender, "?limit=1")
	for _, path := range []string{
		f.path(f.a, "inbox/"+f.sender) + "?cursor=" + url.QueryEscape(oldest.Cursor),
		f.path(f.a, "outbox/"+f.sender) + "?cursor=" + url.QueryEscape(incoming.Cursor),
		f.path(f.a, "outbox/"+f.recipient) + "?cursor=" + url.QueryEscape(oldest.Cursor),
		f.path(f.a, "outbox/"+f.sender) + "?newest=true&cursor=" + url.QueryEscape(oldest.Cursor),
		f.path(f.a, "outbox/"+f.sender) + "?unread=true&cursor=" + url.QueryEscape(oldest.Cursor),
		f.path(f.a, "outbox/"+f.sender) + "?unacknowledged=true&cursor=" + url.QueryEscape(oldest.Cursor),
	} {
		decode(t, f.request(http.MethodGet, path, f.readonly, "", "", nil), http.StatusBadRequest, nil)
	}
	decode(t, f.request(http.MethodGet, f.path(f.a, "outbox/"+f.sender)+"?cursor="+url.QueryEscape(oldest.Cursor), f.clientA, "", "", nil), http.StatusBadRequest, nil)
	decode(t, f.request(http.MethodGet, f.path(f.b, "outbox/"+f.bSender)+"?cursor="+url.QueryEscape(oldest.Cursor), f.clientB, "", "", nil), http.StatusBadRequest, nil)
}

func TestOutgoingReadAuthorizationAdmissionAndMissingReceiptRefusal(t *testing.T) {
	f := setup(t)
	sent := f.send(f.a, f.clientA, f.sender, f.recipient, "private handoff", "", "")
	for _, suffix := range []string{"outbox/" + f.sender, "messages/" + sent.ID + "/delivery"} {
		path := f.path(f.a, suffix)
		errorCode(t, f.request(http.MethodGet, path, "", "", "", nil), http.StatusUnauthorized, "unauthorized")
		errorCode(t, f.request(http.MethodGet, path, f.clientB, "", "", nil), http.StatusForbidden, "forbidden")
		errorCode(t, f.request(http.MethodGet, f.path(f.b, suffix), f.clientB, "", "", nil), http.StatusNotFound, "not_found")
		for _, query := range []string{"?unexpected=true", "?limit=1&limit=2"} {
			errorCode(t, f.request(http.MethodGet, path+query, f.readonly, "", "", nil), http.StatusBadRequest, "validation_failed")
		}
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?unread=maybe", "?newest=true&newest=false", "?cursor=invalid"} {
		decode(t, f.request(http.MethodGet, f.path(f.a, "outbox/"+f.sender)+query, f.readonly, "", "", nil), http.StatusBadRequest, nil)
	}
	errorCode(t, f.request(http.MethodGet, f.path(f.a, "messages/missing/delivery"), f.readonly, "", "", nil), http.StatusNotFound, "not_found")
	// A missing recipient row must not disappear behind an inner join or become
	// a fabricated false receipt, including the unread/pending filtered views.
	if _, err := f.store.DB().Exec("DELETE FROM inbox_recipient_state WHERE project_id=? AND message_id=?", f.a, sent.ID); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"outbox/" + f.sender, "outbox/" + f.sender + "?unread=true", "outbox/" + f.sender + "?unacknowledged=true", "messages/" + sent.ID + "/delivery"} {
		errorCode(t, f.request(http.MethodGet, f.path(f.a, suffix), f.readonly, "", "", nil), http.StatusInternalServerError, "storage_unavailable")
	}
	decode(t, f.request(http.MethodDelete, f.path(f.a, "credentials/"+f.credentialA), f.admin, "", "outbox-revoke", nil), http.StatusOK, nil)
	errorCode(t, f.request(http.MethodGet, f.path(f.a, "outbox/"+f.sender), f.clientA, "", "", nil), http.StatusUnauthorized, "unauthorized")
}
