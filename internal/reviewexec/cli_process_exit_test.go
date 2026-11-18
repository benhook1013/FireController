package reviewexec

import (
	"sync"
	"testing"
	"time"
)

func TestCLIExitedUncertainRetainsPRButFreesAccount(t *testing.T) {
	for _, reason := range []string{"capture_invalid", "capture_retention_failed", "capture_publication_pending", "execution_interrupted", "supervisor_interrupted"} {
		t.Run(reason, func(t *testing.T) {
			f, a, _ := cliPublicationFixture(t, 0, "completed")
			var err error
			a, err = f.e.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", reason, "")
			if err != nil {
				t.Fatal(err)
			}
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			queued := f.enqueue(t, otherAttemptProject, "independent-cli", "cli", 4, 1)
			_, err = f.e.StartAttempt(t.Context(), otherAttemptProject, queued.ID)
			attemptCode(t, err, "review_busy")
			before := attemptState(t, f, a)
			ended := time.Now().UTC()
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := f.e.RecordCLIProcessExit(t.Context(), a.ProjectID, a.ID, ended); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if before != attemptState(t, f, a) {
				t.Fatal("exit changed unresolved review")
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=? AND execution_ended_at<>''`, a.ID) != 1 {
				t.Fatal("PR guard lost")
			}
			pending, err := f.e.pending(t.Context())
			if err != nil || len(pending) != 1 || pending[0].ID != queued.ID {
				t.Fatal("independent PR still blocked", pending, err)
			}
			second := f.start(t, otherAttemptProject, queued)
			// The physical constraint still admits only one live process account-wide.
			if _, err = f.store.DB().Exec(`UPDATE review_execution_reservations SET execution_ended_at='' WHERE attempt_id=?`, a.ID); err == nil {
				t.Fatal("two live execution slots admitted")
			}
			if second.ID == a.ID {
				t.Fatal("attempt reused")
			}
			// Rebuilding the engine preserves the proof and the same-PR hold.
			e, err := New(f.store, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=? AND execution_ended_at<>''`, a.ID) != 1 {
				t.Fatal("restart lost exit proof")
			}
		})
	}
}

func TestCLIExitRejectsUnstartedAndBadTimes(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "not-started-cli", "cli", 2, 1))
	for _, at := range []time.Time{time.Now(), time.Time{}, time.Now().Add(time.Hour)} {
		if err := f.e.RecordCLIProcessExit(t.Context(), a.ProjectID, a.ID, at); err == nil {
			t.Fatal("unproven exit accepted")
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE execution_ended_at=''`) != 1 {
		t.Fatal("unknown process capacity released")
	}
}
