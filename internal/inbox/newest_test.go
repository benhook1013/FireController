package inbox_test

import (
	"net/http"
	"net/url"
	"testing"
)

func TestNewestPendingInboxKeepsSeenWorkAndScopedContinuation(t *testing.T) {
	f := setup(t)
	first := f.send(f.a, f.clientA, f.sender, f.recipient, "old pending", "", "")
	second := f.send(f.a, f.clientA, f.sender, f.recipient, "seen pending", "", "")
	f.transition(f.a, f.clientA, f.recipient, second.ID, "seen", "")
	handled := f.send(f.a, f.clientA, f.sender, f.recipient, "handled", "", "")
	f.transition(f.a, f.clientA, f.recipient, handled.ID, "ack", "")
	third := f.send(f.a, f.clientA, f.sender, f.recipient, "new pending", "", "")
	f.send(f.b, f.clientB, f.bSender, f.bRecipient, "foreign", "", "")
	before := f.eventCount(f.a)
	query := "?newest=true&unacknowledged=true&limit=1"
	page1 := f.inbox(f.a, f.clientA, f.recipient, query)
	if len(page1.Items) != 1 || page1.Items[0].ID != third.ID || !page1.HasMore {
		t.Fatal("newest pending selection failed")
	}
	if f.eventCount(f.a) != before {
		t.Fatal("listing changed durable state")
	}
	// An arrival after the first page belongs to a fresh read/event feed, not an
	// overlapping or reversed continuation through older pending work.
	f.send(f.a, f.clientA, f.sender, f.recipient, "later arrival", "", "")
	page2 := f.inbox(f.a, f.clientA, f.recipient, query+"&cursor="+url.QueryEscape(page1.Cursor))
	if len(page2.Items) != 1 || page2.Items[0].ID != second.ID || !page2.Items[0].Seen || page2.Items[0].Acknowledged || !page2.HasMore {
		t.Fatal("seen-but-unhandled work lost from continuation")
	}
	page3 := f.inbox(f.a, f.clientA, f.recipient, query+"&cursor="+url.QueryEscape(page2.Cursor))
	if len(page3.Items) != 1 || page3.Items[0].ID != first.ID || page3.HasMore {
		t.Fatal("oldest pending work lost")
	}
	base := f.path(f.a, "inbox/"+f.recipient)
	for _, suffix := range []string{
		"?unacknowledged=true&cursor=" + url.QueryEscape(page1.Cursor),
		"?newest=false&unacknowledged=true&cursor=" + url.QueryEscape(page1.Cursor),
		"?newest=true&cursor=" + url.QueryEscape(page1.Cursor),
		"?newest=recent", "?newest=true&newest=true",
	} {
		decode(t, f.request(http.MethodGet, base+suffix, f.clientA, "", "", nil), 400, nil)
	}
	// Default ordering and existing cursors retain their meaning.
	oldest := f.inbox(f.a, f.clientA, f.recipient, "?unacknowledged=true&limit=1")
	if len(oldest.Items) != 1 || oldest.Items[0].ID != first.ID {
		t.Fatal("default ordering changed")
	}
	var snapshot snapshotResponse
	decode(t, f.request(http.MethodGet, base+"/snapshot"+query+"&cursor="+url.QueryEscape(page1.Cursor), f.clientA, f.recipient, "", nil), 200, &snapshot)
	if snapshot.EventCursor == "" || len(snapshot.Inbox.Items) != 1 || snapshot.Inbox.Items[0].ID != second.ID {
		t.Fatal("snapshot cannot continue newest inbox with an independent event watermark")
	}
}
