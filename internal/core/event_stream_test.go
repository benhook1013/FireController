package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func streamFixture(t *testing.T) (*Store, Scope, string) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	token := "synthetic-core-stream-token-1234567"
	sum := sha256.Sum256([]byte(token))
	_, err = s.DB().Exec(`INSERT INTO projects VALUES('project','project',1,'synthetic/project');INSERT INTO workers VALUES('project','worker','worker');INSERT INTO credentials VALUES('credential','principal',?,0,0);INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('credential','project',1,1)`, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://synthetic", nil)
	r.SetPathValue("project_id", "project")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Worker", "worker")
	scope, err := s.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	return s, scope, s.Cursor(scope, messageFeed+scope.WorkerID, "0")
}
func commitStreamEvent(t *testing.T, s *Store, scope Scope, id string) {
	t.Helper()
	_, err := s.Mutate(context.Background(), scope, Mutation{Operation: "test.message", Target: id, Key: id, Request: id}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := AppendEvent(ctx, tx, scope, "message.sent", id, 1, map[string]string{"conversation_id": "conversation", "sender_worker_id": "sender", "recipient_worker_id": scope.WorkerID}); err != nil {
			return nil, err
		}
		return map[string]string{"id": id}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func streamSlots(s *Store) int { s.eventMu.Lock(); defer s.eventMu.Unlock(); return s.eventCount }

func TestStreamRegistersBeforeDeliveryAndCallbacksHoldNoTransaction(t *testing.T) {
	s, scope, cursor := streamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pages := []EventPage{}
	err := s.StreamMessageEvents(ctx, scope, cursor, 1, func(page EventPage) error {
		pages = append(pages, page)
		if len(pages) == 1 {
			if len(page.Items) != 0 || page.Cursor == "" || streamSlots(s) != 1 {
				t.Error("initial ready/subscription missing")
			}
			commitStreamEvent(t, s, scope, "committed-during-callback")
			return nil
		}
		if len(page.Items) != 1 || page.Items[0].ResourceID != "committed-during-callback" {
			t.Error("lost notification between ready and wait")
		}
		cancel()
		return nil
	}, func() error { t.Error("early heartbeat"); return nil })
	if !errors.Is(err, context.Canceled) || len(pages) != 2 || streamSlots(s) != 0 {
		t.Fatalf("stream cancellation/release: %v pages=%d slots=%d", err, len(pages), streamSlots(s))
	}
}

func TestStreamDrainsBoundedPagesAndFailedDeliveryCanReplay(t *testing.T) {
	s, scope, cursor := streamFixture(t)
	for _, id := range []string{"one", "two", "three"} {
		commitStreamEvent(t, s, scope, id)
	}
	rejected := errors.New("synthetic delivery rejection")
	var first EventPage
	err := s.StreamMessageEvents(context.Background(), scope, cursor, 1, func(page EventPage) error { first = page; return rejected }, func() error { return nil })
	if !errors.Is(err, rejected) || len(first.Items) != 1 || streamSlots(s) != 0 {
		t.Fatal("failed delivery did not release")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	seen := []int64{}
	err = s.StreamMessageEvents(ctx, scope, cursor, 1, func(page EventPage) error {
		if len(page.Items) != 1 {
			t.Error("unbounded/empty backlog page")
			cancel()
			return nil
		}
		seen = append(seen, page.Items[0].Sequence)
		if len(seen) < 3 && !page.HasMore {
			t.Error("lost continuation")
		}
		if len(seen) == 3 {
			if page.HasMore {
				t.Error("extra continuation")
			}
			cancel()
		}
		return nil
	}, func() error { return nil })
	if !errors.Is(err, context.Canceled) || len(seen) != 3 || seen[0] != first.Items[0].Sequence || seen[1] <= seen[0] || seen[2] <= seen[1] || streamSlots(s) != 0 {
		t.Fatalf("bounded replay/release: %v %v", err, seen)
	}
}

func TestStreamSharesWaitCapacityAndReleasesCanceledSlots(t *testing.T) {
	s, scope, cursor := streamFixture(t)
	ready := make(chan struct{}, 16)
	done := make(chan error, 16)
	cancels := []context.CancelFunc{}
	for i := 0; i < 16; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		go func() {
			done <- s.StreamMessageEvents(ctx, scope, cursor, 1, func(EventPage) error { ready <- struct{}{}; return nil }, func() error { return nil })
		}()
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	for i := 0; i < 16; i++ {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatal("streams did not become ready")
		}
	}
	if streamSlots(s) != 16 {
		t.Fatal("streams not counted")
	}
	_, err := s.MessageEvents(context.Background(), scope, cursor, 1, time.Millisecond)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "overloaded" {
		t.Fatal("stream/wait capacity not shared")
	}
	called := false
	err = s.StreamMessageEvents(context.Background(), scope, cursor, 1, func(EventPage) error { called = true; return nil }, func() error { return nil })
	if !errors.As(err, &failure) || failure.Code != "overloaded" || called {
		t.Fatal("overloaded stream delivered")
	}
	cancels[0]()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slot not released")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	page, err := s.MessageEvents(ctx, scope, cursor, 1, 10*time.Millisecond)
	if err != nil || !page.TimedOut {
		t.Fatal("released slot not reusable")
	}
	for _, cancel := range cancels {
		cancel()
	}
	for i := 0; i < 15; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("cancel leaked stream")
		}
	}
	if streamSlots(s) != 0 {
		t.Fatal("capacity leaked")
	}
}

func TestStreamHeartbeatReauthorizesWithoutReplayScan(t *testing.T) {
	s, scope, cursor := streamFixture(t)
	stop := errors.New("heartbeat completed")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ready := false
	heartbeats := 0
	const heartbeatInterval = 20 * time.Millisecond
	start := time.Now()
	err := s.streamMessageEvents(ctx, scope, cursor, 1, func(page EventPage) error {
		if ready {
			t.Error("unexpected repeated delivery")
		}
		ready = true
		// If idle operation performs another replay query, it now fails. The
		// heartbeat's authorization-only transaction must not reference events.
		_, err := s.DB().Exec("DROP TABLE events")
		return err
	}, func() error { heartbeats++; return stop }, heartbeatInterval)
	if !errors.Is(err, stop) || heartbeats != 1 || time.Since(start) < heartbeatInterval || streamSlots(s) != 0 {
		t.Fatalf("heartbeat scanned replay or leaked: %v beats=%d", err, heartbeats)
	}
}

func TestStreamHeartbeatRejectsRevokedAccessWithoutReplayScan(t *testing.T) {
	s, scope, cursor := streamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	deliveries, heartbeats := 0, 0
	err := s.streamMessageEvents(ctx, scope, cursor, 1, func(EventPage) error {
		deliveries++
		// Change access without a notification: the next heartbeat must check it
		// independently, and cannot obtain authorization through event replay.
		_, err := s.DB().Exec("DROP TABLE events; DELETE FROM grants")
		return err
	}, func() error { heartbeats++; return nil }, 20*time.Millisecond)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "forbidden" || deliveries != 1 || heartbeats != 0 || streamSlots(s) != 0 {
		t.Fatalf("heartbeat authorization/release: %v deliveries=%d beats=%d", err, deliveries, heartbeats)
	}
}
