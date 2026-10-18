package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func projectCursor(t *testing.T, s *Store, scope Scope) string {
	t.Helper()
	var cursor string
	err := s.Read(context.Background(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		cursor, err = s.ProjectEventCursor(ctx, tx, scope)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}

func commitProjectEvent(t *testing.T, s *Store, scope Scope, kind, id string) {
	t.Helper()
	_, err := s.Mutate(context.Background(), scope, Mutation{Operation: "test.project", Target: id, Key: id, Request: kind}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return map[string]string{"id": id}, AppendEvent(ctx, tx, scope, kind, id, 2, map[string]string{"private": "must not escape"})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProjectEventsSnapshotRaceBoundedReplayAndGaps(t *testing.T) {
	s, scope, _ := streamFixture(t)
	var snapshot string
	// Commit after the snapshot watermark, before subscription registration. The
	// durable first read must see this even though no subscriber received a hint.
	err := s.Read(context.Background(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		snapshot, err = s.ProjectEventCursor(ctx, tx, scope)
		if err == nil {
			commitProjectEvent(t, s, scope, "map.entry_created", "one")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	commitStreamEvent(t, s, scope, "irrelevant-message")
	commitProjectEvent(t, s, scope, "project.presentation_changed", "two")
	commitProjectEvent(t, s, scope, "map.entry_updated", "three")
	commitProjectEvent(t, s, scope, "job.workstream_changed", "four")
	commitProjectEvent(t, s, scope, "job.updated", "irrelevant-tail")
	stop := errors.New("delivery rejected")
	var first ProjectEventPage
	err = s.StreamProjectEvents(context.Background(), scope, snapshot, 1, func(page ProjectEventPage) error { first = page; return stop }, func() error { return nil })
	if !errors.Is(err, stop) || len(first.Items) != 1 || first.Items[0].ResourceID != "one" || !first.HasMore || streamSlots(s) != 0 {
		t.Fatalf("failed-delivery replay: %+v %v", first, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ids []string
	var last ProjectEventPage
	err = s.StreamProjectEvents(ctx, scope, snapshot, 1, func(page ProjectEventPage) error {
		if len(page.Items) != 1 {
			t.Errorf("unbounded page: %+v", page)
			cancel()
			return nil
		}
		item := page.Items[0]
		ids = append(ids, item.ResourceID)
		sequence, e := s.eventPosition(scope, projectFeed, item.EventCursor)
		if e != nil || sequence <= 0 || item.Revision != 2 {
			t.Errorf("bad item: %+v %v", item, e)
		}
		data, _ := json.Marshal(page)
		if strings.Contains(string(data), "private") || strings.Contains(string(data), "payload") || strings.Contains(string(data), "actor_id") {
			t.Error("private event leaked")
		}
		last = page
		if len(ids) == 4 {
			cancel()
		}
		return nil
	}, func() error { return nil })
	if !errors.Is(err, context.Canceled) || strings.Join(ids, ",") != "one,two,three,four" || last.HasMore || last.EventCursor != projectCursor(t, s, scope) || last.EventCursor == last.Items[0].EventCursor {
		t.Fatalf("lost/gap/replay: %v %+v %v", ids, last, err)
	}
	// No matching changes: the ready page advances over gaps without items.
	page, err := s.readProjectEvents(context.Background(), scope, 0, 100)
	if err != nil || len(page.Items) != 4 || page.EventCursor != last.EventCursor {
		t.Fatalf("full page: %+v %v", page, err)
	}
}

func TestProjectEventsRegistrationAndCallbacksOutsideTransaction(t *testing.T) {
	s, scope, _ := streamFixture(t)
	cursor := projectCursor(t, s, scope)
	commitProjectEvent(t, s, scope, "job.updated", "filtered-before-registration")
	initialCursor := projectCursor(t, s, scope)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	err := s.StreamProjectEvents(ctx, scope, cursor, 1, func(page ProjectEventPage) error {
		calls++
		if calls == 1 {
			if streamSlots(s) != 1 || len(page.Items) != 0 || page.EventCursor != initialCursor || page.EventCursor == cursor {
				t.Error("registration/ready missing")
			}
			commitProjectEvent(t, s, scope, "map.entry_created", "callback-write")
		} else {
			if len(page.Items) != 1 || page.Items[0].ResourceID != "callback-write" {
				t.Errorf("lost registered change: %+v", page)
			}
			cancel()
		}
		return nil
	}, func() error { t.Error("early heartbeat"); return nil })
	if !errors.Is(err, context.Canceled) || calls != 2 || streamSlots(s) != 0 {
		t.Fatalf("callbacks/release: %v %d", err, calls)
	}
}

func TestProjectEventsRegisteredKindsWakeAndReplay(t *testing.T) {
	s, scope, _ := streamFixture(t)
	cursor := projectCursor(t, s, scope)
	waiter, release, err := s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, feed: projectFeed})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	after, err := s.eventPosition(scope, projectFeed, cursor)
	if err != nil {
		t.Fatal(err)
	}
	// Subscribe first: commits must use the same kind set for wake hints and
	// durable replay, while an unrelated committed event only advances the head.
	commitProjectEvent(t, s, scope, "project.unknown", "unknown-before")
	if len(waiter.ready) != 0 {
		t.Fatal("unknown event woke project metadata")
	}
	for i, kind := range projectEventKinds {
		id := fmt.Sprintf("registered-%d", i)
		commitProjectEvent(t, s, scope, kind, id)
		if len(waiter.ready) != 1 {
			t.Fatalf("registered kind did not wake: %s", kind)
		}
		<-waiter.ready
		page, err := s.readProjectEvents(context.Background(), scope, after, 1)
		if err != nil || len(page.Items) != 1 || page.HasMore || page.Items[0].EventType != kind || page.Items[0].ResourceID != id || page.Items[0].Revision != 2 {
			t.Fatalf("registered kind replay: %s %+v %v", kind, page, err)
		}
		after, err = s.eventPosition(scope, projectFeed, page.EventCursor)
		if err != nil {
			t.Fatal(err)
		}
	}
	commitProjectEvent(t, s, scope, "project.unknown", "unknown-after")
	if len(waiter.ready) != 0 {
		t.Fatal("unknown event woke project metadata")
	}
	page, err := s.readProjectEvents(context.Background(), scope, after, 1)
	if err != nil || len(page.Items) != 0 || page.HasMore || page.EventCursor != projectCursor(t, s, scope) {
		t.Fatalf("unknown event leaked into replay or lost its watermark: %+v %v", page, err)
	}
}

func TestProjectEventsCursorScopeWorkerIndependenceExpiry(t *testing.T) {
	s, scope, messageCursor := streamFixture(t)
	if _, err := s.DB().Exec("INSERT INTO workers VALUES('project','other','Other')"); err != nil {
		t.Fatal(err)
	}
	cursor := projectCursor(t, s, scope)
	otherWorker := scope
	otherWorker.WorkerID = "other"
	noWorker := scope
	noWorker.WorkerID = ""
	for _, current := range []Scope{scope, otherWorker, noWorker} {
		if projectCursor(t, s, current) != cursor {
			t.Error("worker-dependent project cursor")
		}
		stop := errors.New("ready")
		err := s.StreamProjectEvents(context.Background(), current, cursor, 25, func(ProjectEventPage) error { return stop }, func() error { return nil })
		if !errors.Is(err, stop) {
			t.Fatalf("worker-independent read: %v", err)
		}
	}
	foreignProject := scope
	foreignProject.ProjectID = "elsewhere"
	foreignPrincipal := scope
	foreignPrincipal.PrincipalID = "elsewhere"
	for name, test := range map[string]struct {
		scope  Scope
		cursor string
	}{
		"message": {scope, messageCursor}, "project": {foreignProject, cursor}, "principal": {foreignPrincipal, cursor}, "malformed": {scope, "bad"}, "ahead": {scope, s.Cursor(scope, projectFeed, "99")},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			err := s.StreamProjectEvents(context.Background(), test.scope, test.cursor, 1, func(ProjectEventPage) error { called = true; return nil }, func() error { return nil })
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "validation_failed" || called || streamSlots(s) != 0 {
				t.Fatalf("cursor accepted: %v", err)
			}
		})
	}
	if _, err := s.MessageEvents(context.Background(), scope, cursor, 1, 0); err == nil {
		t.Error("project cursor accepted by message feed")
	}
	commitStreamEvent(t, s, scope, "expired-filtered-gap")
	if _, err := s.DB().Exec("UPDATE events SET created_at=?", time.Now().UTC().Add(-8*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	_, err := s.readProjectEvents(context.Background(), scope, 0, 1)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "resync_required" {
		t.Fatalf("expired gap accepted: %v", err)
	}
	current := projectCursor(t, s, scope)
	position, err := s.eventPosition(scope, projectFeed, current)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.readProjectEvents(context.Background(), scope, position, 1)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("fresh snapshot rejected: %+v %v", page, err)
	}
}

func TestProjectEventsNotificationIsolationAndSharedCapacity(t *testing.T) {
	s, scope, _ := streamFixture(t)
	message, releaseMessage, err := s.addMessageWaiter(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMessage()
	metadata, releaseMetadata, err := s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, feed: projectFeed})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMetadata()
	foreign, releaseForeign, err := s.addEventWaiter(scope, messageTopic{project: "foreign", feed: projectFeed})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseForeign()
	commitStreamEvent(t, s, scope, "message-only")
	if len(message.ready) != 1 || len(metadata.ready) != 0 || len(foreign.ready) != 0 {
		t.Fatal("message woke metadata")
	}
	<-message.ready
	commitProjectEvent(t, s, scope, "map.entry_updated", "metadata-only")
	if len(message.ready) != 0 || len(metadata.ready) != 1 || len(foreign.ready) != 0 {
		t.Fatal("metadata woke wrong namespace/project")
	}
	<-metadata.ready
	// Failed transactions must not publish hints.
	rollback := errors.New("rollback")
	_, err = s.Mutate(context.Background(), scope, Mutation{Operation: "test", Target: "rollback", Key: "rollback", Request: "rollback"}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := AppendEvent(ctx, tx, scope, "map.entry_updated", "rollback", 1, nil); err != nil {
			return nil, err
		}
		return nil, rollback
	})
	if !errors.Is(err, rollback) || len(metadata.ready) != 0 {
		t.Fatal("rolled-back notification")
	}
	releases := []func(){}
	for i := 3; i < 16; i++ {
		_, release, err := s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, feed: projectFeed})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	for _, add := range []func() (*messageWaiter, func(), error){func() (*messageWaiter, func(), error) { return s.addMessageWaiter(scope) }, func() (*messageWaiter, func(), error) {
		return s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, feed: projectFeed})
	}} {
		_, _, err := add()
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "overloaded" {
			t.Fatalf("principal cap not shared: %v", err)
		}
	}
	for _, release := range releases {
		release()
	}
	// Exercise the process cap with distinct principals, independent of the per-principal bound.
	for i := 3; i < 1024; i++ {
		other := scope
		other.PrincipalID = fmt.Sprint("principal-", i)
		_, release, err := s.addEventWaiter(other, messageTopic{project: scope.ProjectID, feed: projectFeed})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	_, _, err = s.addMessageWaiter(Scope{PrincipalID: "new-principal"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "overloaded" {
		t.Fatalf("process cap not shared: %v", err)
	}
}

func TestProjectEventsIdleAuthorizationAndRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			s, scope, _ := streamFixture(t)
			cursor := projectCursor(t, s, scope)
			stop := errors.New("heartbeat")
			beats := 0
			err := s.streamProjectEvents(context.Background(), scope, cursor, 25, func(ProjectEventPage) error {
				query := "DROP TABLE events"
				if revoke {
					query += ";DELETE FROM grants"
				}
				_, err := s.DB().Exec(query)
				return err
			}, func() error {
				beats++
				// Authorization transaction is closed before transport callbacks.
				_, err := s.Mutate(context.Background(), scope, Mutation{Operation: "heartbeat", Target: "test", Key: "heartbeat", Request: "test"}, func(context.Context, *sql.Tx) (any, error) { return "ok", nil })
				if err != nil {
					return err
				}
				return stop
			}, 10*time.Millisecond)
			if revoke {
				var failure *Error
				if !errors.As(err, &failure) || failure.Code != "forbidden" || beats != 0 {
					t.Fatalf("auth heartbeat: %v %d", err, beats)
				}
			} else if !errors.Is(err, stop) || beats != 1 {
				t.Fatalf("idle scan or callback: %v %d", err, beats)
			}
			if streamSlots(s) != 0 {
				t.Fatal("subscription leaked")
			}
		})
	}
}
