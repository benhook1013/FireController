package core

import (
	"context"
	"strconv"
	"time"
)

type messageTopic struct{ project, worker, feed string }
type eventChangesKey struct{}
type eventChanges struct {
	topics  map[messageTopic]struct{}
	revoked bool
}
type messageWaiter struct {
	principal string
	ready     chan struct{}
}

// Notifications are hints after commit; durable rows and signed cursors own
// replay. A full hint channel coalesces changes without blocking the writer.
func (s *Store) notifyEvents(changes *eventChanges) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	notify := func(group map[*messageWaiter]struct{}) {
		for waiter := range group {
			select {
			case waiter.ready <- struct{}{}:
			default:
			}
		}
	}
	if changes.revoked {
		for _, group := range s.eventGroups {
			notify(group)
		}
		return
	}
	for topic := range changes.topics {
		notify(s.eventGroups[topic])
	}
}

func (s *Store) addMessageWaiter(scope Scope) (*messageWaiter, func(), error) {
	return s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, worker: scope.WorkerID, feed: messageFeed})
}

func (s *Store) addEventWaiter(scope Scope, topic messageTopic) (*messageWaiter, func(), error) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.eventCount >= 1024 || s.principalWaiters[scope.PrincipalID] >= 16 {
		return nil, nil, Fail("overloaded", "The bounded event wait capacity is occupied; retry later.", 503)
	}
	if s.eventGroups == nil {
		s.eventGroups = make(map[messageTopic]map[*messageWaiter]struct{})
		s.principalWaiters = make(map[string]int)
	}
	if s.eventGroups[topic] == nil {
		s.eventGroups[topic] = make(map[*messageWaiter]struct{})
	}
	w := &messageWaiter{principal: scope.PrincipalID, ready: make(chan struct{}, 1)}
	s.eventGroups[topic][w] = struct{}{}
	s.eventCount++
	s.principalWaiters[scope.PrincipalID]++
	return w, func() {
		s.eventMu.Lock()
		defer s.eventMu.Unlock()
		delete(s.eventGroups[topic], w)
		if len(s.eventGroups[topic]) == 0 {
			delete(s.eventGroups, topic)
		}
		s.eventCount--
		s.principalWaiters[scope.PrincipalID]--
		if s.principalWaiters[scope.PrincipalID] == 0 {
			delete(s.principalWaiters, scope.PrincipalID)
		}
	}, nil
}

// MessageEvents returns a bounded page after a scoped snapshot/replay cursor.
// Waiting occupies no database transaction and performs no provider work.
func (s *Store) MessageEvents(ctx context.Context, scope Scope, cursor string, limit int, wait time.Duration) (EventPage, error) {
	if scope.WorkerID == "" || cursor == "" || limit < 1 || limit > 100 || wait < 0 || wait > 4*time.Second {
		return EventPage{}, Fail("validation_failed", "Use a selected worker, snapshot cursor, limit 1-100 and wait 0-4000 milliseconds.", 400)
	}
	after, err := s.messageEventPosition(scope, cursor)
	if err != nil {
		return EventPage{}, err
	}
	if wait > 0 {
		waiter, release, err := s.addMessageWaiter(scope)
		if err != nil {
			return EventPage{}, err
		}
		defer release()
		return s.waitMessageEvents(ctx, scope, after, limit, wait, waiter)
	}
	return s.readMessageEvents(ctx, scope, after, limit)
}

func (s *Store) messageEventPosition(scope Scope, cursor string) (int64, error) {
	return s.eventPosition(scope, messageFeed+scope.WorkerID, cursor)
}

func (s *Store) eventPosition(scope Scope, feed, cursor string) (int64, error) {
	position, err := s.ParseCursor(scope, feed, cursor)
	if err != nil {
		return 0, err
	}
	after, err := strconv.ParseInt(position, 10, 64)
	if err != nil || after < 0 {
		return 0, Fail("validation_failed", "The event cursor position is invalid.", 400)
	}
	return after, nil
}

func (s *Store) waitMessageEvents(ctx context.Context, scope Scope, after int64, limit int, wait time.Duration, waiter *messageWaiter) (EventPage, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		page, err := s.readMessageEvents(ctx, scope, after, limit)
		if err != nil || len(page.Items) > 0 {
			return page, err
		}
		// Advancing over filtered changes is safe internally; no received or
		// processed cursor is acknowledged on the caller's behalf.
		after, err = s.messageEventPosition(scope, page.Cursor)
		if err != nil {
			return EventPage{}, err
		}
		select {
		case <-ctx.Done():
			return EventPage{}, storageError(ctx.Err())
		case <-waiter.ready:
		case <-deadline.C:
			page, err = s.readMessageEvents(ctx, scope, after, limit)
			page.TimedOut = err == nil && len(page.Items) == 0
			return page, err
		}
	}
}
