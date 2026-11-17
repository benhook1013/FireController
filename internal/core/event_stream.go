package core

import (
	"context"
	"database/sql"
	"time"
)

// StreamMessageEvents uses commit notifications as coalesced hints and durable
// reads as delivery authority. Callbacks run after read transactions finish.
// Returning from a callback does not acknowledge processing on the client's behalf.
// Assignment attention is opt-in and shares the same selected-worker cursor.
func (s *Store) StreamMessageEvents(ctx context.Context, scope Scope, cursor string, limit int, deliver func(EventPage) error, heartbeat func() error, includeAssignments ...bool) error {
	return s.streamMessageEvents(ctx, scope, cursor, limit, deliver, heartbeat, 15*time.Second, includeAssignments...)
}

func (s *Store) streamMessageEvents(ctx context.Context, scope Scope, cursor string, limit int, deliver func(EventPage) error, heartbeat func() error, heartbeatInterval time.Duration, includeAssignments ...bool) error {
	if scope.WorkerID == "" || cursor == "" || limit < 1 || limit > 100 || deliver == nil || heartbeat == nil {
		return Fail("validation_failed", "Use a selected worker, snapshot cursor, limit 1-100 and stream callbacks.", 400)
	}
	return s.streamEvents(ctx, scope, cursor, messageFeed+scope.WorkerID,
		messageTopic{project: scope.ProjectID, worker: scope.WorkerID, feed: messageFeed},
		func(after int64) (streamEventPage, error) {
			page, err := s.readMessageEvents(ctx, scope, after, limit, len(includeAssignments) > 0 && includeAssignments[0])
			return streamEventPage{page.Cursor, page.HasMore, len(page.Items), func() error { return deliver(page) }}, err
		}, heartbeat, heartbeatInterval)
}

// Both feeds share subscription ownership, authorization heartbeats and delivery
// advancement. Typed readers retain each feed's independent wire and filtering.
type streamEventPage struct {
	cursor  string
	hasMore bool
	count   int
	deliver func() error
}

func (s *Store) streamEvents(ctx context.Context, scope Scope, cursor, feed string, topic messageTopic, read func(int64) (streamEventPage, error), heartbeat func() error, heartbeatInterval time.Duration) error {
	after, err := s.eventPosition(scope, feed, cursor)
	if err != nil {
		return err
	}
	waiter, release, err := s.addEventWaiter(scope, topic)
	if err != nil {
		return err
	}
	defer release()
	heartbeats := time.NewTicker(heartbeatInterval)
	defer heartbeats.Stop()
	initial := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := read(after)
		if err != nil {
			return err
		}
		if initial || page.count > 0 || page.cursor != cursor {
			if err = page.deliver(); err != nil {
				return err
			}
			// Advance only after delivery. The client still owns its processed
			// cursor and may reconnect with an older one for at-least-once replay.
			cursor = page.cursor
			after, err = s.eventPosition(scope, feed, cursor)
			if err != nil {
				return err
			}
		}
		initial = false
		if page.hasMore {
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
				s.touchWorkerContact(scope)
			}
		}
	read:
	}
}
