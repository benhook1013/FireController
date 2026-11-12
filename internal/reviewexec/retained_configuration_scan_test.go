package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestCurrentConfigurationCorruptionIsCandidateLocal(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, damage := range []string{"malformed", "unknown", "blob", "shape"} {
			t.Run(channel+"/"+damage, func(t *testing.T) {
				f := supervisorFixture(t)
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				bad := f.enqueue(t, attemptProject, "bad-current", channel, 2, 1)
				healthy := f.enqueue(t, otherAttemptProject, "healthy-current", channel, 2, 1)
				switch damage {
				case "malformed":
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json='private-broken' WHERE project_id=?`, attemptProject)
				case "unknown":
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.unknown_private',1) WHERE project_id=?`, attemptProject)
				case "blob":
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json=CAST(body_json AS BLOB) WHERE project_id=?`, attemptProject)
				case "shape":
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.queue',json('[]')) WHERE project_id=?`, attemptProject)
				}
				before, err := readOperation(t.Context(), f.store.DB(), attemptProject, bad.ID)
				if err != nil {
					t.Fatal(err)
				}
				seen := map[string]int{}
				driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
					seen[o.ID]++
					return Preparation{}, core.Fail("review_evidence_pending", "synthetic hold", 409)
				}}
				for range 3 {
					if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
						t.Fatal(err)
					}
				}
				after, err := readOperation(t.Context(), f.store.DB(), attemptProject, bad.ID)
				if err != nil || encoded(before) != encoded(after) || seen[bad.ID] != 0 || seen[healthy.ID] != 1 || driver.calls.Load() != 0 {
					t.Fatal("corruption escaped candidate boundary", seen, err)
				}
			})
		}
	}
}

func TestSourceConfigurationDecodeClassificationPreservesQueryErrors(t *testing.T) {
	f := supervisorFixture(t)
	admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.unknown_private',1) WHERE project_id=?`, attemptProject)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := sourceConfiguration(t.Context(), tx, attemptProject, 1); !isStoredUnavailable(err, storedConfigurationHistory) {
		t.Fatal("decode not classified", err)
	}
	if _, err := sourceConfiguration(t.Context(), tx, attemptProject, 99); !errors.Is(err, sql.ErrNoRows) || isStoredUnavailable(err, storedConfigurationHistory) {
		t.Fatal("query error reclassified", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sourceConfiguration(ctx, tx, attemptProject, 1); !errors.Is(err, context.Canceled) || isStoredUnavailable(err, storedConfigurationHistory) {
		t.Fatal("cancellation reclassified", err)
	}
}

func TestPendingScanDeadlineRetainsProgressWithoutChangingAdmission(t *testing.T) {
	for _, probe := range []bool{false, true} {
		for _, channel := range []string{"cli", "hosted"} {
			t.Run(channel+map[bool]string{false: "/ordinary", true: "/draft-worker"}[probe], func(t *testing.T) {
				f := supervisorFixture(t)
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				bad := f.enqueue(t, attemptProject, "slow-prefix", channel, 2, 1)
				healthy := f.enqueue(t, otherAttemptProject, "later-ready", channel, 2, 1)
				admissionSQL(t, f, `UPDATE review_operations SET created_at='2020-01-01T00:00:00Z' WHERE id=?`, bad.ID)
				admissionSQL(t, f, `UPDATE review_execution_config SET body_json='private-broken' WHERE project_id=?`, attemptProject)
				// The clock is consulted once for snapshot time, then for the typed
				// candidate refusal. Delay that bounded validation past its read budget.
				calls := 0
				f.e.schedulingNow = func() time.Time {
					calls++
					if calls == 2 {
						time.Sleep(2100 * time.Millisecond)
					}
					return time.Now().UTC()
				}
				if _, err := f.e.pendingForWorker(t.Context(), probe); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("fixture did not exhaust scan budget", err)
				}
				f.e.schedulingNow = nil
				got, err := f.e.pendingForWorker(t.Context(), probe)
				if err != nil || len(got) != 1 || got[0].ID != healthy.ID {
					t.Fatal("timed-out prefix starved later candidate", got, err)
				}
				// A later full scan wraps back to the oldest candidate. It still cannot
				// dispatch the corrupt request and does not mutate or cancel it.
				got, err = f.e.pendingForWorker(t.Context(), probe)
				if err != nil || len(got) != 1 || got[0].ID != healthy.ID {
					t.Fatal("scan failed to wrap", got, err)
				}
				o, err := readOperation(t.Context(), f.store.DB(), attemptProject, bad.ID)
				if err != nil || o.State != "queued" || o.AttemptID != "" {
					t.Fatal("scan changed retained intent", o, err)
				}
				if a, err := f.e.StartAttempt(t.Context(), attemptProject, bad.ID); err == nil || a.ID != "" {
					t.Fatal("scan bypassed admission", a, err)
				}
			})
		}
	}
}

func TestPendingScanCursorConcurrentWorkers(t *testing.T) {
	f := supervisorFixture(t)
	f.enqueue(t, attemptProject, "cursor-race", "cli", 2, 1)
	var wg sync.WaitGroup
	for _, probe := range []bool{false, true} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				if _, err := f.e.pendingForWorker(t.Context(), probe); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestPausedQueuedStatusDirectsControlWithoutMutatingWait(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		f := supervisorFixture(t)
		r := f.enqueue(t, attemptProject, "paused-status", channel, 2, 1)
		timerControl(t, f, r.ID, "pause", "pause-status")
		scope := f.scope(t, attemptProject, f.writer, false)
		status, err := f.e.ReadOperationStatus(t.Context(), scope, r.ID)
		if err != nil || status.NextAction != "resume_or_cancel" || status.Wait == nil || !status.Wait.Paused || status.State != "queued" {
			t.Fatal(status, err)
		}
		timerControl(t, f, r.ID, "resume", "resume-status")
		status, err = f.e.ReadOperationStatus(t.Context(), scope, r.ID)
		if err != nil || status.NextAction != "wait" {
			t.Fatal(status, err)
		}
	}
}
