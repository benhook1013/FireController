package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

func TestPendingScanDoesNotSkipReturnedBatch(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(fmt.Sprint(probe), func(t *testing.T) {
			f := supervisorFixture(t)
			seed := f.enqueue(t, attemptProject, "batch-seed", "cli", 2, 1)
			for i := range 260 {
				admissionSQL(t, f, `INSERT INTO review_operations (id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations WHERE id=?`, fmt.Sprintf("batch-%03d", i), seed.ID)
			}
			first, err := f.e.pendingForWorker(t.Context(), probe)
			if err != nil || len(first) != 256 {
				t.Fatal(len(first), err)
			}
			var seen []string
			driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
				seen = append(seen, o.ID)
				return Preparation{}, core.Fail("review_evidence_pending", "fixture hold", 409)
			}}
			for range 2 {
				if err := f.e.processNextForWorker(t.Context(), map[string]Driver{"test-capability": driver}, probe); err != nil {
					t.Fatal(err)
				}
			}
			if len(seen) != 2 || seen[0] != first[0].ID || seen[1] != first[1].ID {
				t.Fatal("processing skipped unprocessed batch", seen)
			}
			if driver.calls.Load() != 0 {
				t.Fatal("fixture unexpectedly dispatched provider work")
			}

		})
	}
}

func TestPendingScanPartialDeadlineRetainsReturnedCandidate(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(fmt.Sprint(probe), func(t *testing.T) {
			f := supervisorFixture(t)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			ready := f.enqueue(t, attemptProject, "first-ready", "cli", 2, 1)
			bad := f.enqueue(t, otherAttemptProject, "slow-after-ready", "cli", 2, 1)
			admissionSQL(t, f, `UPDATE review_operations SET created_at='2020-01-01T00:00:00Z' WHERE id=?`, ready.ID)
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json='broken' WHERE project_id=?`, otherAttemptProject)
			calls := 0
			f.e.schedulingNow = func() time.Time {
				calls++
				if calls == 2 && !probe || calls == 3 && probe {
					time.Sleep(2100 * time.Millisecond)
				}
				return time.Now().UTC()
			}
			first, err := f.e.pendingForWorker(t.Context(), probe)
			if err != nil || len(first) != 1 || first[0].ID != ready.ID {
				t.Fatal("partial deadline", first, err)
			}
			f.e.schedulingNow = nil
			next, err := f.e.pendingForWorker(t.Context(), probe)
			if err != nil || len(next) != 1 || next[0].ID != ready.ID {
				t.Fatal("skipped returned ready candidate", next, err, bad.ID)
			}
		})
	}
}

func TestSourceRefreshDamagedConfigurationIsCandidateLocal(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprint(retained), func(t *testing.T) {
			f := supervisorFixture(t)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			bad := refreshEnqueue(t, f, "damaged-refresh", refreshRequest())
			raw, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, otherAttemptProject, f.other, false), "healthy-refresh", refreshRequest())
			if err != nil {
				t.Fatal(err)
			}
			var healthy Receipt
			if err = json.Unmarshal(raw, &healthy); err != nil {
				t.Fatal(err)
			}
			if retained {
				f.configure(t, attemptProject, "test-capability", 1, false)
			}
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json='broken' WHERE project_id=? AND revision=1`, attemptProject)
			schedule := newSourceRefreshSchedule()
			for range 2 {
				if _, err := f.e.refreshNextScheduled(t.Context(), nil, schedule); err != nil {
					t.Fatal("one damaged project stopped source worker", err)
				}
			}
			b := refreshRead(t, f, bad.ID)
			if b.State != "held" || b.Reason != "source_configuration_changed" {
				t.Fatal("damaged candidate not held", b)
			}
			var state, reason string
			if err = f.store.DB().QueryRow(`SELECT state,reason FROM review_source_refreshes WHERE id=?`, healthy.ID).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if state != "held" || reason != "source_reader_unavailable" {
				t.Fatal("healthy project did not reach its independent reader check", state, reason)
			}
		})
	}
}

func TestSourceRefreshOperationalFailuresRemainFatal(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			f := supervisorFixture(t)
			receipt := refreshEnqueue(t, f, "operational-refresh", refreshRequest())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			} else {
				admissionSQL(t, f, `ALTER TABLE review_execution_config RENAME TO unavailable_configuration`)
			}
			if _, err := f.e.refreshNext(ctx, nil); err == nil {
				t.Fatal("operational failure became local hold")
			}
			var state string
			if err := f.store.DB().QueryRow(`SELECT state FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&state); err != nil || state != "queued" {
				t.Fatal("operational failure changed retained refresh", state, err)
			}
		})
	}
}
