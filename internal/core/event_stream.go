package core

import (
	"context"
	"database/sql"
	"time"
)

// StreamMessageEvents uses commit notifications as coalesced hints and durable
// reads as delivery authority. Callbacks run after read transactions finish.
// Returning from a callback does not acknowledge processing on the client's behalf.
func (s *Store) StreamMessageEvents(ctx context.Context, scope Scope, cursor string, limit int, deliver func(EventPage) error, heartbeat func() error) error {
	if scope.WorkerID == "" || cursor == "" || limit < 1 || limit > 100 || deliver == nil || heartbeat == nil {
		return Fail("validation_failed", "Use a selected worker, snapshot cursor, limit 1-100 and stream callbacks.", 400)
	}
	after, err := s.messageEventPosition(scope, cursor)
	if err != nil {
		return err
	}
	waiter, release, err := s.addMessageWaiter(scope)
	if err != nil {
		return err
	}
	defer release()
	heartbeats := time.NewTicker(15 * time.Second)
	defer heartbeats.Stop()
	initial := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := s.readMessageEvents(ctx, scope, after, limit)
		if err != nil {
			return err
		}
		if initial || len(page.Items) > 0 || page.Cursor != cursor {
			if err = deliver(page); err != nil {
				return err
			}
			// Advance only after delivery. The client still owns its processed
			// cursor and may reconnect with an older one for at-least-once replay.
			cursor = page.Cursor
			after, err = s.messageEventPosition(scope, cursor)
			if err != nil {
				return err
			}
		}
		initial = false
		if page.HasMore {
			continue
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-waiter.ready:
				goto read
			case <-heartbeats.C:
				// Check current authorization, without replay/domain scans, and
				// close the read transaction before touching a slow transport.
				if err = s.Read(ctx, scope, func(context.Context, *sql.Tx) error { return nil }); err != nil {
					return err
				}
				if err = heartbeat(); err != nil {
					return err
				}
			}
		}
	read:
	}
}
