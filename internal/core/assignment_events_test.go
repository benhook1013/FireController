package core

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func commitAssignmentEvent(t *testing.T, s *Store, scope Scope, recipient, key string, rollback bool) {
	t.Helper()
	_, err := s.Mutate(t.Context(), scope, Mutation{Operation: "test.assignment", Target: key, Key: key, Request: key}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := AppendEvent(ctx, tx, scope, "job.assigned", key, 2, map[string]string{"recipient_worker_id": recipient, "sender_worker_id": "dispatcher"}); err != nil {
			return nil, err
		}
		if rollback {
			return nil, errors.New("synthetic rollback")
		}
		return map[string]string{"id": key}, nil
	})
	if (err != nil) != rollback {
		t.Fatal("unexpected assignment commit result", err)
	}
}

func TestAssignmentEventsWakeActiveStreamAndReplayOnlySelectedSuffix(t *testing.T) {
	s, scope, cursor := streamFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ready, delivery, done := make(chan struct{}), make(chan EventPage, 4), make(chan error, 1)
	go func() {
		first := true
		done <- s.StreamMessageEvents(ctx, scope, cursor, 10, func(page EventPage) error {
			if first {
				first = false
				close(ready)
				return nil
			}
			delivery <- page
			return nil
		}, func() error { return nil }, true)
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("assignment stream never became ready")
	}
	commitAssignmentEvent(t, s, scope, "other-worker", "irrelevant-assignment", false)
	commitAssignmentEvent(t, s, scope, scope.WorkerID, "rolled-back-assignment", true)
	select {
	case page := <-delivery:
		t.Fatal("irrelevant or failed assignment woke the selected stream", page)
	case <-time.After(30 * time.Millisecond):
	}
	commitAssignmentEvent(t, s, scope, scope.WorkerID, "relevant-assignment", false)
	select {
	case page := <-delivery:
		if len(page.Items) != 1 || page.Items[0].ResourceID != "relevant-assignment" {
			t.Fatal("active assignment delivery lost recipient scope", page)
		}
	case <-ctx.Done():
		t.Fatal("committed assignment did not wake the active stream")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || streamSlots(s) != 0 {
		t.Fatal("assignment stream cancellation retained a subscription", err)
	}
	selected, err := s.MessageEvents(t.Context(), scope, cursor, 10, 0, true)
	if err != nil || len(selected.Items) != 1 || selected.Items[0].Type != "job.assigned" {
		t.Fatal("durable assignment replay failed", selected, err)
	}
	legacy, err := s.MessageEvents(t.Context(), scope, cursor, 10, 0)
	if err != nil || len(legacy.Items) != 0 || legacy.Cursor != selected.Cursor {
		t.Fatal("default message-only selection changed", legacy, err)
	}
	advanced, err := s.MessageEvents(t.Context(), scope, legacy.Cursor, 10, 0, true)
	if err != nil || len(advanced.Items) != 0 {
		t.Fatal("enabling assignments credited an already-consumed suffix", advanced, err)
	}
}
