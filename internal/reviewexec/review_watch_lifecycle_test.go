package reviewexec

import (
	"testing"
)

// Use real lifecycle producers: synthetic watch events cannot detect missing
// transactional emissions when an operation leaves queued state.
func TestReviewWatchNativeLifecycle(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := f.enqueue(t, attemptProject, "watch-lifecycle", channel, 2, 1)
			scope := operationReadScope(t, f)
			o := ReviewWatchOptions{PullNumber: 2, Channel: channel, Limit: 10}
			initial, err := f.e.WatchReviews(t.Context(), scope, o, 0)
			if err != nil {
				t.Fatal(err)
			}
			o.After = initial.EventCursor
			check := func(state string, revision int64) {
				t.Helper()
				result, err := f.e.WatchReviews(t.Context(), scope, o, 0)
				if err != nil || result.TimedOut || len(result.Changes) != 1 {
					t.Fatalf("%s: %+v %v", state, result, err)
				}
				c := result.Changes[0]
				if c.EventType != "review.operation_updated" || c.Revision != revision || c.Operation.ID != r.ID || c.Operation.State != state || c.Operation.Revision != revision {
					t.Fatalf("%s: %+v", state, c)
				}
				// A lost response can replay the same successful cursor.
				replay, err := f.e.WatchReviews(t.Context(), scope, o, 0)
				if err != nil || len(replay.Changes) != 1 || replay.Changes[0].Sequence != c.Sequence {
					t.Fatalf("replay: %+v %v", replay, err)
				}
				o.After = result.EventCursor
			}
			a := f.start(t, attemptProject, r)
			check("preparing", 2)
			intent, state := "", "active"
			if channel == "hosted" {
				intent, state = "watch-intent", "posting"
			}
			a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), intent)
			if err != nil {
				t.Fatal(err)
			}
			check(state, 3)
			revision := int64(3)
			if channel == "hosted" {
				a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_confirmed", "external-watch")
				if err != nil {
					t.Fatal(err)
				}
				revision++
				check("active", revision)
			}
			if err = f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			revision++
			check("uncertain", revision)
			if err = f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			quiet, err := f.e.WatchReviews(t.Context(), scope, o, 0)
			if err != nil || !quiet.TimedOut || len(quiet.Changes) != 0 {
				t.Fatalf("recovery replay: %+v %v", quiet, err)
			}
		})
	}
}

func TestReviewWatchLifecycleEventFailureRollsBack(t *testing.T) {
	for _, phase := range []string{"preparing", "admitted", "advanced", "recovered"} {
		t.Run(phase, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := f.enqueue(t, attemptProject, "watch-rollback", "cli", 2, 1)
			var a Attempt
			var err error
			if phase != "preparing" {
				a = f.start(t, attemptProject, r)
			}
			if phase == "advanced" || phase == "recovered" {
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
				if err != nil {
					t.Fatal(err)
				}
			}
			before := ""
			if phase != "preparing" {
				before = attemptState(t, f, a)
			}
			events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			admissionSQL(t, f, `CREATE TRIGGER reject_lifecycle_event BEFORE INSERT ON events WHEN NEW.event_type='review.operation_updated' BEGIN SELECT RAISE(ABORT,'event failure'); END`)
			switch phase {
			case "preparing":
				_, err = f.e.StartAttempt(t.Context(), attemptProject, r.ID)
			case "admitted":
				_, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
			case "advanced":
				_, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "interrupted", "")
			case "recovered":
				err = f.e.RecoverInterrupted(t.Context())
			}
			if err == nil {
				t.Fatal("event failure committed lifecycle mutation")
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
				t.Fatal("event journal changed")
			}
			if phase == "preparing" {
				if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE id=? AND state='queued' AND revision=1 AND attempt_id=''`, r.ID) != 1 {
					t.Fatal("start was not rolled back")
				}
			} else if after := attemptState(t, f, a); after != before {
				t.Fatalf("mutation survived rollback: %s -> %s", before, after)
			}
		})
	}
}
