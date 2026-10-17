package core

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestLiveProjectBackgroundCommitWakesRegisteredStream(t *testing.T) {
	s, scope, _ := streamFixture(t)
	cursor := projectCursor(t, s, scope)
	foreign, releaseForeign, err := s.addEventWaiter(scope, messageTopic{project: "foreign", feed: projectFeed})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseForeign()
	message, releaseMessage, err := s.addMessageWaiter(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMessage()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stop := errors.New("delivered committed background observation")
	calls := 0
	err = s.StreamProjectEvents(ctx, scope, cursor, 10, func(page ProjectEventPage) error {
		calls++
		if calls == 1 {
			if len(page.Items) != 0 {
				t.Fatalf("unexpected initial history: %+v", page)
			}
			return s.BackgroundWrite(ctx, func(writeCtx context.Context, tx *sql.Tx) error {
				if err := AppendEvent(writeCtx, tx, scope, "review.observed", "background-snapshot", 7, map[string]string{"secret": "never transported"}); err != nil {
					return err
				}
				// Real concurrent reader isolation and no precommit wakeup. The
				// stream is registered before invoking this transport callback.
				uncommitted, err := s.readProjectEvents(ctx, scope, 0, 10)
				if err != nil {
					return err
				}
				if len(uncommitted.Items) != 0 {
					t.Error("background event exposed before commit")
				}
				s.eventMu.Lock()
				for _, group := range s.eventGroups {
					for waiter := range group {
						if len(waiter.ready) != 0 {
							t.Error("background hint exposed before commit")
						}
					}
				}
				s.eventMu.Unlock()
				return nil
			})
		}
		if len(page.Items) != 1 || page.Items[0].EventType != "review.observed" || page.Items[0].ResourceID != "background-snapshot" || page.Items[0].Revision != 7 {
			t.Fatalf("lost committed review change: %+v", page)
		}
		return stop
	}, func() error { t.Error("unexpected heartbeat instead of commit wakeup"); return nil })
	if !errors.Is(err, stop) || calls != 2 {
		t.Fatalf("background live delivery: calls=%d err=%v", calls, err)
	}
	if len(foreign.ready) != 0 || len(message.ready) != 0 || streamSlots(s) != 2 {
		t.Fatal("background commit woke unrelated stream or leaked subscription")
	}
}

func TestLiveProjectBackgroundFailureNeverPublishes(t *testing.T) {
	for _, failure := range []string{"callback", "cancellation", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s, scope, _ := streamFixture(t)
			waiter, release, err := s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, feed: projectFeed})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if failure == "commit" {
				if _, err := s.writer.Exec(`CREATE TABLE live_project_guard(project_id TEXT REFERENCES projects(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rejected := errors.New("reject background transaction")
			err = s.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
				if err := AppendEvent(ctx, tx, scope, "review.observed", "must-rollback", 1, nil); err != nil {
					return err
				}
				switch failure {
				case "callback":
					return rejected
				case "cancellation":
					cancel()
				case "commit":
					_, err := tx.ExecContext(ctx, `INSERT INTO live_project_guard VALUES('missing-project')`)
					return err
				}
				return nil
			})
			if err == nil || failure == "callback" && !errors.Is(err, rejected) {
				t.Fatalf("failure not preserved: %v", err)
			}
			page, readErr := s.readProjectEvents(context.Background(), scope, 0, 10)
			if readErr != nil || len(page.Items) != 0 || len(waiter.ready) != 0 {
				t.Fatalf("failed background write exposed event or hint: %+v %v hints=%d", page, readErr, len(waiter.ready))
			}
			release()
			if streamSlots(s) != 0 {
				t.Fatal("failed background write leaked subscription")
			}
		})
	}
}
