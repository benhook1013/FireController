package inbox_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
)

func pendingContextScope(t *testing.T, f *fixture) core.Scope {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("project_id", f.a)
	r.Header.Set("Authorization", "Bearer "+f.clientA)
	r.Header.Set("X-FC-Worker", f.recipient)
	scope, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestPendingContextSharesCallerSnapshotAndFollowingEventReplay(t *testing.T) {
	f := setup(t)
	first := f.send(f.a, f.clientA, f.sender, f.recipient, "before snapshot", "", "")
	f.transition(f.a, f.clientA, f.recipient, first.ID, "seen", "")
	scope := pendingContextScope(t, f)
	var pending inbox.PendingPage
	var cursor, afterID string
	err := f.store.Read(t.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		// Establish the caller's snapshot before another ordinary API write.
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_messages WHERE project_id=?`, f.a).Scan(&count); err != nil {
			return err
		}
		after := f.send(f.a, f.clientA, f.sender, f.recipient, "after snapshot", "", "")
		afterID = after.ID
		var err error
		pending, cursor, err = inbox.ReadPendingContext(ctx, tx, f.store, scope, 20)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Items) != 1 || pending.Items[0].ID != first.ID || !pending.Items[0].Seen || pending.Items[0].Acknowledged || pending.HasMore || cursor == "" {
		t.Fatal("pending context did not use existing snapshot or include seen/unhandled message")
	}
	w := f.request("GET", f.path(f.a, "message-events")+"?cursor="+url.QueryEscape(cursor), f.clientA, f.recipient, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), afterID) || strings.Contains(w.Body.String(), "after snapshot") {
		t.Fatal("snapshot watermark lost following event or included private body", w.Body.String())
	}
	if f.providerCalls != 2 {
		t.Fatal("pending context invoked provider")
	}
}

func TestPendingContextRejectsMissingHandlingStateInsteadOfEmptySuccess(t *testing.T) {
	f := setup(t)
	m := f.send(f.a, f.clientA, f.sender, f.recipient, "must remain visible", "", "")
	if _, err := f.store.DB().Exec(`DELETE FROM inbox_recipient_state WHERE project_id=? AND message_id=?`, f.a, m.ID); err != nil {
		t.Fatal(err)
	}
	events := f.eventCount(f.a)
	err := f.store.Read(t.Context(), pendingContextScope(t, f), func(ctx context.Context, tx *sql.Tx) error {
		_, _, err := inbox.ReadPendingContext(ctx, tx, f.store, pendingContextScope(t, f), 20)
		return err
	})
	var failure *core.Error
	if err == nil || !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
		t.Fatalf("missing handling state became empty context: %v", err)
	}
	if f.eventCount(f.a) != events {
		t.Fatal("failed pending context wrote events")
	}
}
