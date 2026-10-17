package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func publicProjectionHint(t *testing.T, hints <-chan struct{}, expected bool) {
	t.Helper()
	select {
	case <-hints:
		if !expected {
			t.Fatal("unexpected public projection hint")
		}
	default:
		if expected {
			t.Fatal("committed public projection hint missing")
		}
	}
}

func TestPublicProjectionNotificationsOnlyMarkedCommitsAndFeedIsolation(t *testing.T) {
	s, scope, _ := streamFixture(t)
	hints, release, err := s.SubscribePublicProjection(scope.ProjectID, "anonymous-client")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	foreign, releaseForeign, err := s.SubscribePublicProjection("foreign-project", "anonymous-client")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseForeign()
	private, releasePrivate, err := s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, feed: projectFeed})
	if err != nil {
		t.Fatal(err)
	}
	defer releasePrivate()
	messages, releaseMessages, err := s.addMessageWaiter(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMessages()
	for i, kind := range []string{"job.created", "review.observed", "map.entry_updated", "note.created"} {
		commitProjectEvent(t, s, scope, kind, fmt.Sprint("private-", i))
		publicProjectionHint(t, hints, false)
		publicProjectionHint(t, foreign, false)
		publicProjectionHint(t, private.ready, true)
	}
	commitStreamEvent(t, s, scope, "private-message")
	publicProjectionHint(t, hints, false)
	publicProjectionHint(t, messages.ready, true)
	var beforeEvents int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	err = s.BackgroundWrite(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if err := MarkPublicProjectionChanged(ctx, scope.ProjectID); err != nil {
			return err
		}
		publicProjectionHint(t, hints, false)
		return MarkPublicProjectionChanged(ctx, scope.ProjectID)
	})
	if err != nil {
		t.Fatal(err)
	}
	publicProjectionHint(t, hints, true)
	publicProjectionHint(t, hints, false)
	publicProjectionHint(t, foreign, false)
	publicProjectionHint(t, private.ready, false)
	publicProjectionHint(t, messages.ready, false)
	var afterEvents int
	if err := s.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&afterEvents); err != nil || afterEvents != beforeEvents {
		t.Fatal("mark inserted private/public event rows", beforeEvents, afterEvents, err)
	}
	_, err = s.Mutate(t.Context(), scope, Mutation{Operation: "test.public", Target: "published", Key: "published", Request: "selected"}, func(ctx context.Context, _ *sql.Tx) (any, error) {
		return map[string]string{"outcome": "published"}, MarkPublicProjectionChanged(ctx, scope.ProjectID)
	})
	if err != nil {
		t.Fatal(err)
	}
	publicProjectionHint(t, hints, true)
	// A slow subscriber gets one coalesced signal, not a public activity count.
	for range 3 {
		if err := s.BackgroundWrite(t.Context(), func(ctx context.Context, _ *sql.Tx) error { return MarkPublicProjectionChanged(ctx, scope.ProjectID) }); err != nil {
			t.Fatal(err)
		}
	}
	publicProjectionHint(t, hints, true)
	publicProjectionHint(t, hints, false)
	// Revocation uses the existing all-topic wake path. This contains no public
	// event or data; the publication owner must suppress unchanged projections.
	commitProjectEvent(t, s, scope, "credentials.revoked", "private-revocation")
	publicProjectionHint(t, hints, true)
	publicProjectionHint(t, foreign, true)
}

func TestPublicProjectionSubscriptionsShareGlobalStreamCapacity(t *testing.T) {
	s, scope, _ := streamFixture(t)
	_, releasePrivate, err := s.addMessageWaiter(scope)
	if err != nil {
		t.Fatal(err)
	}
	var privateOnce sync.Once
	closePrivate := func() { privateOnce.Do(releasePrivate) }
	defer closePrivate()
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := range 1023 {
		_, release, err := s.SubscribePublicProjection(scope.ProjectID, fmt.Sprint("client-", i))
		if err != nil {
			t.Fatal("shared capacity filled early", i, err)
		}
		releases = append(releases, release)
	}
	_, _, err = s.SubscribePublicProjection(scope.ProjectID, "one-too-many")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "overloaded" {
		t.Fatal("public subscriptions bypassed global stream capacity", err)
	}
	if _, _, err := s.addMessageWaiter(scope); err == nil {
		t.Fatal("private subscriptions bypassed shared capacity")
	}
	for _, release := range releases {
		release()
	}
	closePrivate()
	if streamSlots(s) != 0 {
		t.Fatal("global capacity leaked after cleanup", streamSlots(s))
	}
}

func TestPublicProjectionNotificationsRollbackCancelAndFailedCommitStayQuiet(t *testing.T) {
	for _, mode := range []string{"rollback", "cancel", "commit-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, scope, _ := streamFixture(t)
			hints, release, err := s.SubscribePublicProjection(scope.ProjectID, "anonymous-client")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			// A deferred foreign-key violation succeeds as a statement and fails
			// only at COMMIT, exercising the real transaction boundary.
			if _, err := s.DB().Exec(`CREATE TABLE public_projection_parent(id INTEGER PRIMARY KEY);CREATE TABLE public_projection_child(parent_id INTEGER REFERENCES public_projection_parent(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rollback := errors.New("controlled rollback")
			err = s.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
				if err := MarkPublicProjectionChanged(ctx, scope.ProjectID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO public_projection_parent VALUES(1)`); err != nil {
					return err
				}
				switch mode {
				case "rollback":
					return rollback
				case "cancel":
					cancel()
					return nil
				default:
					_, err := tx.ExecContext(ctx, `INSERT INTO public_projection_child VALUES(2)`)
					if err != nil {
						t.Error("failure occurred before commit", err)
					}
					return err
				}
			})
			if err == nil {
				t.Fatal("failed transaction unexpectedly committed")
			}
			publicProjectionHint(t, hints, false)
			var rows int
			if err := s.DB().QueryRow(`SELECT count(*) FROM public_projection_parent`).Scan(&rows); err != nil || rows != 0 {
				t.Fatal("failed transaction retained rows", rows, err)
			}
		})
	}
}

func TestPublicProjectionSubscriptionBoundsNamespacesAndIdempotentCleanup(t *testing.T) {
	s, scope, _ := streamFixture(t)
	for _, input := range []string{"", "bad key", "bad:key", "../project", "é", strings.Repeat("x", 129)} {
		if _, _, err := s.SubscribePublicProjection(input, "client"); err == nil {
			t.Fatal("invalid project accepted", input)
		}
		if _, _, err := s.SubscribePublicProjection(scope.ProjectID, input); err == nil {
			t.Fatal("invalid client accepted", input)
		}
	}
	if err := MarkPublicProjectionChanged(t.Context(), scope.ProjectID); err == nil {
		t.Fatal("mark accepted outside write collector")
	}
	if err := MarkPublicProjectionChanged(nil, scope.ProjectID); err == nil {
		t.Fatal("mark accepted nil context")
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := range 16 {
		project := scope.ProjectID
		if i%2 != 0 {
			project = "foreign-project"
		}
		_, release, err := s.SubscribePublicProjection(project, scope.PrincipalID)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, _, err := s.SubscribePublicProjection(scope.ProjectID, scope.PrincipalID); err == nil {
		t.Fatal("anonymous per-client cap did not span projects")
	}
	// Matching raw names do not spend an authenticated principal's own slots.
	private, releasePrivate, err := s.addMessageWaiter(scope)
	if err != nil || private == nil {
		t.Fatal("anonymous accounting collided with authenticated principal", err)
	}
	releasePrivate()
	var cleanup sync.WaitGroup
	for _, release := range releases {
		for range 3 {
			cleanup.Go(release)
		}
	}
	cleanup.Wait()
	if streamSlots(s) != 0 {
		t.Fatal("cleanup was not idempotent", streamSlots(s))
	}
	s.eventMu.Lock()
	principals, groups := len(s.principalWaiters), len(s.eventGroups)
	s.eventMu.Unlock()
	if principals != 0 || groups != 0 {
		t.Fatal("subscription accounting leaked", principals, groups)
	}
}
