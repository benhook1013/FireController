package inbox_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

type snapshotResponse struct {
	Inbox       page   `json:"inbox"`
	EventCursor string `json:"event_cursor"`
}

func TestIncomingSnapshotUsesFilteredBoundedInboxAndReadOnlyWatermark(t *testing.T) {
	f := setup(t)
	first := f.send(f.a, f.clientA, f.sender, f.recipient, "first synthetic message", "", "")
	second := f.send(f.a, f.clientA, f.sender, f.recipient, "second synthetic message", "", "")
	f.send(f.a, f.clientA, f.sender, f.recipient, "third synthetic message", "", "")
	f.transition(f.a, f.clientA, f.recipient, first.ID, "seen", "")

	eventsBefore := f.eventCount(f.a)
	var seenBefore int
	if err := f.store.DB().QueryRow("SELECT seen FROM inbox_recipient_state WHERE project_id=? AND message_id=? AND worker_id=?", f.a, second.ID, f.recipient).Scan(&seenBefore); err != nil {
		t.Fatal(err)
	}
	path := f.path(f.a, "inbox/"+f.recipient+"/snapshot") + "?unread=true&unacknowledged=true&limit=1"
	w := f.request("GET", path, f.clientA, f.recipient, "", nil)
	var got snapshotResponse
	decode(t, w, http.StatusOK, &got)
	if len(got.Inbox.Items) != 1 || got.Inbox.Items[0].ID != second.ID || !got.Inbox.HasMore || got.Inbox.Cursor == "" {
		t.Fatalf("bounded filtered inbox=%+v", got.Inbox)
	}
	if got.EventCursor == "" {
		t.Fatal("missing signed incoming-message watermark")
	}

	// The watermark is signed for this worker feed and captures the current
	// project high-water mark while the page is read. No event or receipt state
	// may be changed merely by taking a snapshot.
	req := httptest.NewRequest("GET", path, nil)
	req.SetPathValue("project_id", f.a)
	req.Header.Set("Authorization", "Bearer "+f.clientA)
	req.Header.Set("X-FC-Worker", f.recipient)
	scope, err := f.store.Scope(req, "read")
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.store.ParseCursor(scope, "events:incoming-messages:"+f.recipient, got.EventCursor)
	if err != nil {
		t.Fatalf("watermark scope: %v", err)
	}
	watermark, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		t.Fatalf("watermark value %q: %v", after, err)
	}
	var highwater int64
	if err := f.store.DB().QueryRow("SELECT coalesce(max(sequence),0) FROM events WHERE project_id=?", f.a).Scan(&highwater); err != nil {
		t.Fatal(err)
	}
	if watermark != highwater {
		t.Fatalf("snapshot watermark=%d project highwater=%d", watermark, highwater)
	}
	var seenAfter int
	if err := f.store.DB().QueryRow("SELECT seen FROM inbox_recipient_state WHERE project_id=? AND message_id=? AND worker_id=?", f.a, second.ID, f.recipient).Scan(&seenAfter); err != nil {
		t.Fatal(err)
	}
	if f.eventCount(f.a) != eventsBefore || seenAfter != seenBefore {
		t.Fatal("snapshot mutated event or recipient state")
	}
	if f.providerCalls != 2 {
		t.Fatal("snapshot called repository provider")
	}
}

func TestIncomingSnapshotRequiresSelectedWorkerAndRejectsInvalidFilters(t *testing.T) {
	f := setup(t)
	base := f.path(f.a, "inbox/"+f.recipient+"/snapshot")
	for _, tc := range []struct {
		path, token, worker string
		status              int
	}{
		{path: base, token: f.clientA, status: http.StatusForbidden},
		{path: base, token: f.clientA, worker: f.sender, status: http.StatusForbidden},
		{path: f.path(f.a, "inbox/missing/snapshot"), token: f.clientA, worker: "missing", status: http.StatusNotFound},
		{path: f.path(f.b, "inbox/"+f.bRecipient+"/snapshot"), token: f.clientA, worker: f.bRecipient, status: http.StatusForbidden},
	} {
		w := f.request("GET", tc.path, tc.token, tc.worker, "", nil)
		if w.Code != tc.status {
			t.Fatalf("snapshot %s worker=%q: HTTP %d want %d: %s", tc.path, tc.worker, w.Code, tc.status, w.Body.String())
		}
	}
	for _, query := range []string{"?unsupported=true", "?unread=1", "?unread=true&unread=false", "?limit=0", "?cursor=invalid"} {
		w := f.request("GET", base+query, f.clientA, f.recipient, "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid snapshot query accepted %s: HTTP %d %s", query, w.Code, w.Body.String())
		}
	}
	if f.providerCalls != 2 {
		t.Fatal("snapshot validation called repository provider")
	}
}

func TestSnapshotPageCursorRemainsInboxFilterSpecific(t *testing.T) {
	f := setup(t)
	f.send(f.a, f.clientA, f.sender, f.recipient, "first synthetic message", "", "")
	f.send(f.a, f.clientA, f.sender, f.recipient, "second synthetic message", "", "")
	path := f.path(f.a, "inbox/"+f.recipient+"/snapshot") + "?limit=1"
	var got snapshotResponse
	decode(t, f.request("GET", path, f.clientA, f.recipient, "", nil), http.StatusOK, &got)
	if !got.Inbox.HasMore || got.Inbox.Cursor == "" {
		t.Fatalf("snapshot did not preserve bounded inbox cursor: %+v", got.Inbox)
	}
	query := "?cursor=" + url.QueryEscape(got.Inbox.Cursor)
	var next snapshotResponse
	decode(t, f.request("GET", f.path(f.a, "inbox/"+f.recipient+"/snapshot")+query, f.clientA, f.recipient, "", nil), http.StatusOK, &next)
	if len(next.Inbox.Items) != 1 || next.Inbox.Items[0].ID == got.Inbox.Items[0].ID || next.Inbox.HasMore {
		t.Fatalf("snapshot next page=%+v", next.Inbox)
	}
}
