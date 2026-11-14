package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
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

func TestPendingScanDoesNotSkipReturnedBatch(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(fmt.Sprint(probe), func(t *testing.T) {
			f := supervisorFixture(t)
			seed := f.enqueue(t, attemptProject, "batch-seed", "cli", 2, 1)
			for i := range 260 {
				admissionSQL(t, f, `INSERT INTO review_operations (id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations WHERE id=?`, fmt.Sprintf("batch-%03d", i), seed.ID)
			}
			first, err := f.e.pendingForWorker(t.Context(), probe)
			// A deadline may produce a partial batch. This regression needs two
			// candidates to prove processing preserves the unprocessed remainder,
			// not a full batch whose size depends on runner throughput.
			if err != nil || len(first) < 2 || len(first) > 256 {
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

func TestSettlementRecoveryPreservesQueryBeforeObservation(t *testing.T) {
	f, a, _ := cooldownObservationFixture(t)
	admissionSQL(t, f, `DELETE FROM review_hosted_observations WHERE attempt_id=?`, a.ID)
	skipped, err := f.e.reconcilePreparingDrivers(t.Context(), nil)
	if err != nil || !skipped[a.ID] {
		t.Fatal("query-only preparation lost", skipped, err)
	}
}

func TestSettlementDamagedWaitDoesNotBlockOtherRecovery(t *testing.T) {
	f, a, _ := cooldownObservationFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "healthy-recovery", "cli", 2, 1))
	admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,?,'damaged',0)`, a.OperationID, a.ID)
	skipped, err := f.e.reconcilePreparingDrivers(t.Context(), nil)
	if err != nil || !skipped[a.ID] {
		t.Fatal("damaged wait stopped startup", skipped, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id='`+a.ID+`'`) != 1 {
		t.Fatal("reservation lost")
	}
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovered, err := readAttempt(t.Context(), f.store.DB(), other.ProjectID, other.ID)
	if err != nil || recovered.State != "cancelled" {
		t.Fatal("unrelated recovery blocked", recovered, err)
	}
	retained, err := readAttempt(t.Context(), f.store.DB(), a.ProjectID, a.ID)
	if err != nil || retained.State != "preparing" {
		t.Fatal("damaged attempt lost", retained, err)
	}
}

func TestSettlementNullHostedQueryIDIsTyped(t *testing.T) {
	f, a, _, _ := queryObservationFixture(t)
	admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET id=NULL WHERE attempt_id=?`, a.ID)
	o, err := readOperation(t.Context(), f.store.DB(), a.ProjectID, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readHostedQuery(t.Context(), f.store.DB(), a, o)
	if !isStoredUnavailable(err, storedHostedQuery) {
		t.Fatalf("not typed: %v", err)
	}
}

func TestSettlementStaleOrdinarySnapshotCannotClaim(t *testing.T) {
	f := supervisorFixture(t)
	id := f.enqueue(t, attemptProject, "stale-snapshot", "hosted", 2, 1).ID
	o, err := readOperation(t.Context(), f.store.DB(), attemptProject, id)
	if err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE review_operations SET revision=revision+1 WHERE id=?`, id)
	_, err = f.e.startAttempt(t.Context(), attemptProject, id, 0, o.Revision)
	if err == nil {
		t.Fatal("stale snapshot claimed")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("attempt created")
	}
}

func TestSettlementHistoricalQueryReplayAlreadySupported(t *testing.T) {
	f, a, _, _ := queuedCooldown(t)
	records, err := f.e.ReadHostedObservations(t.Context(), a.ProjectID, a.ID)
	if err != nil || len(records) == 0 {
		t.Fatal(records, err)
	}
	for _, record := range records {
		if record.Input.Kind != "allowance_query" {
			continue
		}
		got, err := f.e.RecordHostedObservation(t.Context(), a.ProjectID, a.ID, record.Input)
		if err != nil || got.ID != record.ID {
			t.Fatal("historical replay rejected", got, err)
		}
		return
	}
	t.Fatal("query observation absent")
}

func TestSettlementRawPendingUsesCursorIndex(t *testing.T) {
	f := supervisorFixture(t)
	rows, err := f.store.DB().Query(`EXPLAIN QUERY PLAN SELECT o.project_id,o.id,o.channel,o.capability_fingerprint,o.created_at,o.reason FROM review_operations o WHERE o.state='queued' AND (o.created_at,o.id)>(?,?) ORDER BY o.created_at,o.id`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "review_operations_pending") {
			indexed = true
		}
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Fatal("raw cursor requires unbounded sort", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("raw cursor index unused")
	}
}

func TestSourceRefreshCompletionConfigurationCorruptionIsLocal(t *testing.T) {
	f := supervisorFixture(t)
	r := refreshEnqueue(t, f, "completion-corruption", refreshRequest())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	releaseReader := sync.OnceFunc(func() { close(release) })
	defer releaseReader()
	reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return reviewprovider.Repository{}, ctx.Err()
		}
		return reviewprovider.Repository{ID: id, FullName: "owner/repo"}, nil
	}}
	done := make(chan error, 1)
	go func() {
		_, err := f.e.refreshNextScheduled(ctx, map[string]TargetReader{"test-capability": reader}, newSourceRefreshSchedule())
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("refresh worker ended before entering repository read", err)
	case <-ctx.Done():
		t.Fatal("refresh worker did not enter repository read", ctx.Err())
	}
	admissionSQL(t, f, `UPDATE review_execution_config SET body_json='broken' WHERE project_id=?`, attemptProject)
	releaseReader()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("completion stopped shared worker", err)
		}
	case <-ctx.Done():
		t.Fatal("refresh worker did not finish after release", ctx.Err())
	}
	got := refreshRead(t, f, r.ID)
	if got.State != "held" || got.Reason != "source_configuration_changed" {
		t.Fatal(got)
	}
}
func TestPreparationRecoveryBadFlagsRemainCandidateLocal(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprint(retained), func(t *testing.T) {
			f := supervisorFixture(t)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "bad-flags", "hosted", 2, 1))
			if retained {
				retainTimerCooldown(t, f, a, 6*time.Minute)
			}
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-flags", "cli", 2, 1))
			admissionSQL(t, f, `PRAGMA ignore_check_constraints=ON`)
			admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=2 WHERE id=?`, a.OperationID)
			admissionSQL(t, f, `PRAGMA ignore_check_constraints=OFF`)
			skipped, err := f.e.reconcilePreparingDrivers(t.Context(), nil)
			if err != nil || skipped[a.ID] == retained {
				t.Fatal("preflight replay selection", skipped, err)
			}
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, err := readAttempt(t.Context(), f.store.DB(), other.ProjectID, other.ID)
			if err != nil || got.State != "cancelled" {
				t.Fatal(got, err)
			}
			got, err = readAttempt(t.Context(), f.store.DB(), a.ProjectID, a.ID)
			if err != nil || got.State != "preparing" {
				t.Fatal(got, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id='`+a.ID+`'`) != 1 {
				t.Fatal("damaged reservation lost")
			}

		})
	}
}

func TestPreparationRecoveryMalformedAnchorRemainsCandidateLocal(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "bad-anchor", "hosted", 2, 1))
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-anchor", "cli", 2, 1))
	admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json='private-broken' WHERE id=?`, a.ID)
	for range 2 {
		if err := f.e.RecoverInterrupted(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readAttempt(t.Context(), f.store.DB(), other.ProjectID, other.ID)
	if err != nil || got.State != "cancelled" {
		t.Fatal("unrelated preparation was not recovered", got, err)
	}
	var state, anchor string
	if err := f.store.DB().QueryRow(`SELECT state,anchor_json FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&state, &anchor); err != nil {
		t.Fatal(err)
	}
	if state != "preparing" || anchor != "private-broken" {
		t.Fatal("damaged attempt changed", state, anchor)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id='`+a.ID+`'`) != 1 {
		t.Fatal("damaged reservation lost")
	}
}

func TestPendingScanInvalidOperationFlagsRemainCandidateLocal(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, flag := range []string{"allow_out_of_order", "override_file_limit"} {
			t.Run(channel+"/"+flag, func(t *testing.T) {
				f := supervisorFixture(t)
				f.configure(t, otherAttemptProject, "other-account", 0, false)
				bad := f.enqueue(t, attemptProject, "bad-flag", channel, 2, 1)
				successor := f.enqueue(t, attemptProject, "blocked-successor", channel, 4, 1)
				healthy := f.enqueue(t, otherAttemptProject, "healthy-flag", channel, 2, 1)
				admissionSQL(t, f, `PRAGMA ignore_check_constraints=ON`)
				admissionSQL(t, f, `UPDATE review_operations SET `+flag+`=2,created_at='2020-01-01T00:00:00Z' WHERE id=?`, bad.ID)
				admissionSQL(t, f, `PRAGMA ignore_check_constraints=OFF`)
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
				var state, attempt string
				var rawFlag int
				if err := f.store.DB().QueryRow(`SELECT state,attempt_id,`+flag+` FROM review_operations WHERE id=?`, bad.ID).Scan(&state, &attempt, &rawFlag); err != nil {
					t.Fatal(err)
				}
				if state != "queued" || attempt != "" || rawFlag != 2 || seen[bad.ID] != 0 || seen[successor.ID] != 0 || seen[healthy.ID] != 1 || driver.calls.Load() != 0 {
					t.Fatal("corrupt operation affected independent work or was admitted", state, attempt, rawFlag, seen)
				}
				got, err := readOperation(t.Context(), f.store.DB(), attemptProject, successor.ID)
				if err != nil || got.State != "queued" || got.AttemptID != "" {
					t.Fatal("damaged predecessor was bypassed", got, err)
				}
			})
		}
	}
}

func TestUnavailableOperationSkipDoesNotHideStorageErrors(t *testing.T) {
	f := supervisorFixture(t)
	for _, err := range []error{sql.ErrNoRows, context.Canceled, context.DeadlineExceeded, errors.New("private storage error"), newStoredUnavailable(storedReviewAdmission, "private anchor error")} {
		if f.e.skipUnavailableOperation(t.Context(), Operation{ID: "operation"}, err) {
			t.Fatal("non-identity error skipped", err)
		}
	}
}

func TestOperationIdentityIsolationIsTypedAndBounded(t *testing.T) {
	now := time.Now()
	e := &Engine{schedulingNow: func() time.Time { return now }}
	op := Operation{ID: "review_operation_candidate", ProjectID: "project"}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, sql.ErrTxDone, errors.New("database failure"), core.Fail("storage_unavailable", "Stored source refresh is unavailable.", 500)} {
		if e.skipUnavailableOperation(t.Context(), op, err) || e.skipUnavailableOperation(t.Context(), op, fmt.Errorf("wrapped: %w", err)) {
			t.Fatal("real storage error isolated", err)
		}
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	unavailable := fmt.Errorf("private wrapper: %w", newStoredUnavailable(storedOperationIdentity, "private retained history"))
	for i := range 300 {
		op.ID = fmt.Sprintf("review_operation_%d", i)
		if !e.skipUnavailableOperation(t.Context(), op, unavailable) {
			t.Fatal("typed history not isolated")
		}
	}
	if len(e.configurationFailureNext) != 256 || strings.Count(output.String(), "\n") != 256 || strings.Contains(output.String(), "private") {
		t.Fatal("unbounded or unsafe diagnostics", len(e.configurationFailureNext))
	}
	now = now.Add(30 * time.Second)
	if !e.skipUnavailableOperation(t.Context(), op, unavailable) || len(e.configurationFailureNext) != 1 || strings.Count(output.String(), "\n") != 257 {
		t.Fatal("diagnostic throttle did not expire")
	}
}
