package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type settlementLock struct {
	*sql.Tx
	cancel context.CancelFunc
}

func (lock *settlementLock) Rollback() error {
	err := lock.Tx.Rollback()
	lock.cancel()
	return err
}

func settlementWriterLock(ctx context.Context, f *attemptFixture, operation string) (*settlementLock, error) {
	// This is a separate owned SQLite connection, not Store.BackgroundWrite's
	// admission slot. It produces a real SQLITE_BUSY at the journal writer.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	acquireCtx, stopAcquisition := context.WithTimeout(ctx, time.Second)
	defer stopAcquisition()
	for {
		if err := acquireCtx.Err(); err != nil {
			return nil, fmt.Errorf("acquire owned SQLite writer: %w", err)
		}
		// The fault belongs to the test, not the adapter: returning from Prepare
		// or Dispatch must not cancel this separate writer before settlement.
		ownedCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tx, err := f.store.DB().BeginTx(ownedCtx, nil)
		if err == nil {
			_, err = tx.ExecContext(acquireCtx, `UPDATE review_operations SET reason=reason WHERE id=?`, operation)
			if err == nil {
				return &settlementLock{Tx: tx, cancel: cancel}, nil
			}
			_ = tx.Rollback()
		}
		cancel()
		// The supervisor can briefly own the writer while starting another
		// attempt. Retry only SQLite contention, with a fresh transaction;
		// this is fixture acquisition, not a retry of any adapter action.
		var coded interface{ Code() int }
		if !errors.As(err, &coded) || coded.Code()&255 != 5 && coded.Code()&255 != 6 {
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-acquireCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("acquire owned SQLite writer after contention: %w", acquireCtx.Err())
		case <-timer.C:
		}
	}
}

type settlementLockResult struct {
	lock *settlementLock
	err  error
}

func settlementReceiveLock(t *testing.T, locks <-chan settlementLockResult) *settlementLock {
	t.Helper()
	select {
	case result := <-locks:
		if result.err != nil {
			t.Fatalf("fake adapter could not acquire its owned SQLite lock: %v", result.err)
		}
		t.Cleanup(func() { _ = result.lock.Rollback() })
		return result.lock
	case <-time.After(3 * time.Second):
		t.Fatal("fake adapter did not acquire its owned SQLite lock")
		return nil
	}
}

func TestSettlementWriterLockWaitsForOwnedContention(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	r := f.enqueue(t, attemptProject, "fixture-lock-contention", "cli", 2, 1)
	first, err := settlementWriterLock(context.Background(), f, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Rollback() })
	// Force contention rather than depending on the supervisor's timing. The
	// second acquisition must wait through actual SQLite busy responses.
	released := make(chan error, 1)
	go func() {
		time.Sleep(80 * time.Millisecond)
		released <- first.Rollback()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second, err := settlementWriterLock(ctx, f, r.ID)
	if err != nil {
		t.Fatal("owned writer did not recover after contention", err)
	}
	t.Cleanup(func() { _ = second.Rollback() })
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	// Returning from the adapter and cancelling its context must leave the
	// independently owned fault in place for journal settlement to encounter.
	cancel()
	if _, err := second.ExecContext(context.Background(), `UPDATE review_operations SET reason=reason WHERE id=?`, r.ID); err != nil {
		t.Fatal("adapter cancellation released its independently owned lock", err)
	}
}

func TestSettlementConfigurationReplayAfterCapabilityRemovalOrNarrowing(t *testing.T) {
	for _, mode := range []string{"removed", "narrowed"} {
		t.Run(mode, func(t *testing.T) {
			f := newAttemptFixture(t)
			revision := int64(0)
			c := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}
			scope := f.scope(t, attemptProject, f.admin, true)
			first, err := f.e.Configure(context.Background(), scope, "recover-configuration", c)
			if err != nil {
				t.Fatal(err)
			}
			initialEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			var capabilities []Capability
			if mode == "narrowed" {
				capabilities = []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"cli"}}}
			}
			f.e, err = New(f.store, capabilities)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := f.e.Configure(context.Background(), scope, "recover-configuration", c)
			if err != nil || !bytes.Equal(first, replay) {
				t.Fatal("exact successful configuration retry did not retain its original receipt", err)
			}
			_, err = f.e.Configure(context.Background(), scope, "new-unavailable-configuration", c)
			attemptCode(t, err, "validation_failed")
			changed := c
			changed.Mode = "stacked"
			_, err = f.e.Configure(context.Background(), scope, "recover-configuration", changed)
			attemptCode(t, err, "idempotency_conflict")
			malformed := c
			malformed.Queue = []int64{0}
			_, err = f.e.Configure(context.Background(), scope, "recover-configuration", malformed)
			attemptCode(t, err, "validation_failed")
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != initialEvents {
				t.Fatal("replay or rejected new configuration changed durable effects")
			}
			if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE principal_id=?`, scope.PrincipalID); err != nil {
				t.Fatal(err)
			}
			_, err = f.e.Configure(context.Background(), scope, "recover-configuration", c)
			attemptCode(t, err, "unauthorized")
		})
	}
}

func TestSettlementTransientSQLiteLockRetriesOnlyLocalOutcome(t *testing.T) {
	for _, phase := range []string{"preparation", "trigger", "finish"} {
		t.Run(phase, func(t *testing.T) {
			f := supervisorFixture(t)
			locks := make(chan settlementLockResult, 1)
			triggerResult := make(chan error, 1)
			releaseDispatch := make(chan struct{})
			driver := &controlledDriver{}
			if phase == "preparation" {
				driver.prepare = func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
					tx, err := settlementWriterLock(ctx, f, o.ID)
					locks <- settlementLockResult{lock: tx, err: err}
					if err != nil {
						return Preparation{}, err
					}
					return Preparation{}, errors.New("synthetic private preflight failure")
				}
			} else {
				driver.dispatch = func(ctx context.Context, o Operation, _ Attempt, record func(context.Context, string) error) (Result, error) {
					tx, err := settlementWriterLock(ctx, f, o.ID)
					locks <- settlementLockResult{lock: tx, err: err}
					if err != nil {
						return Result{}, err
					}
					if phase == "trigger" {
						err := record(ctx, "retained_trigger_123")
						triggerResult <- err
						if err != nil {
							return Result{ExternalID: "retained_trigger_123"}, err
						}
						select {
						case <-releaseDispatch:
							return Result{State: "completed", Reason: "capture_retained", ExternalID: "retained_trigger_123"}, nil
						case <-ctx.Done():
							return Result{}, ctx.Err()
						}
					}
					return Result{State: "completed", Reason: "capture_retained"}, nil
				}
			}
			stop := runSupervisor(t, f, driver)
			channel := "cli"
			if phase == "trigger" {
				channel = "hosted"
			}
			r := f.enqueue(t, attemptProject, "transient-"+phase, channel, 2, 1)
			tx := settlementReceiveLock(t, locks)
			// Both initial journal attempts see the held SQLite writer before
			// its controlled release. Only local persistence is retried.
			time.Sleep(80 * time.Millisecond)
			state := supervisorOperationState(t, f, r.ID)
			if phase == "preparation" && state != "preparing" || phase == "trigger" && state != "posting" || phase == "finish" && state != "active" {
				t.Fatal("journal unexpectedly advanced through an owned write lock", state)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if phase == "trigger" {
				select {
				case err := <-triggerResult:
					if err != nil {
						t.Fatal("transient trigger write did not recover", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("trigger retry did not finish")
				}
				waitOperationState(t, f, r.ID, "active")
				var external, intent string
				if err := f.store.DB().QueryRow(`SELECT external_id,intent FROM review_execution_attempts WHERE operation_id=?`, r.ID).Scan(&external, &intent); err != nil || external != "retained_trigger_123" || intent == "" {
					t.Fatal("trigger and intent were not durable while Dispatch remained blocked", err)
				}
				close(releaseDispatch)
			}
			want, dispatches := "completed", int32(1)
			if phase == "preparation" {
				want, dispatches = "held", 0
			}
			waitOperationState(t, f, r.ID, want)
			stop()
			if driver.prepares.Load() != 1 || driver.calls.Load() != dispatches || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("local retry repeated adapter work or leaked terminal reservation")
			}
		})
	}
}

func TestSettlementPermanentSQLiteFailureStopsSupervisorAndCancelsOtherWork(t *testing.T) {
	f := supervisorFixture(t)
	locks := make(chan settlementLockResult, 1)
	otherEntered, otherCancelled := make(chan struct{}), make(chan struct{})
	driver := &controlledDriver{dispatch: func(ctx context.Context, o Operation, _ Attempt, _ func(context.Context, string) error) (Result, error) {
		if o.Channel == "hosted" {
			close(otherEntered)
			<-ctx.Done()
			close(otherCancelled)
			return Result{}, ctx.Err()
		}
		tx, err := settlementWriterLock(ctx, f, o.ID)
		locks <- settlementLockResult{lock: tx, err: err}
		if err != nil {
			return Result{}, err
		}
		return Result{State: "completed", Reason: "capture_retained"}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done, err := f.e.Start(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
	if err != nil {
		cancel()
		t.Fatal("settlement supervisor startup:", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("failed supervisor leaked owned workers")
		}
	})
	f.enqueue(t, attemptProject, "cooperative-other-work", "hosted", 2, 1)
	select {
	case <-otherEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("independent channel did not start")
	}
	failed := f.enqueue(t, attemptProject, "permanent-settlement-failure", "cli", 2, 1)
	tx := settlementReceiveLock(t, locks)
	select {
	case err := <-done:
		attemptCode(t, err, "review_persistence_failed")
		if strings.Contains(err.Error(), "SQLITE") || strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "private") {
			t.Fatal("settlement failure exposed raw diagnostics")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("permanent settlement error was not visible and finite")
	}
	select {
	case <-otherCancelled:
	default:
		t.Fatal("fatal persistence failure did not cancel other driver work")
	}
	if f.e.running.Load() || driver.prepares.Load() != 2 || driver.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 2 || supervisorOperationState(t, f, failed.ID) != "active" {
		t.Fatal("fatal write failure redispatched, lost reservation, or retained supervisor ownership")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Startup recovery after storage is usable retains uncertainty and never
	// dispatches either external action again.
	stop := runSupervisor(t, f, driver)
	waitOperationState(t, f, failed.ID, "uncertain")
	stop()
	if driver.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 2 {
		t.Fatal("recovery after fatal settlement spent another action")
	}
}

func TestSettlementReconcilesAlreadyCommittedExactCAS(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "ambiguous-commit", "cli", 2, 1))
	a, err := f.e.Admit(context.Background(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := f.e.Advance(context.Background(), attemptProject, a.ID, a.Revision, "completed", "capture_retained", "")
	if err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, committed)
	// Present the original CAS after its write is durably present, as a caller
	// with an ambiguous commit response would do. No commit fault is fabricated.
	recovered, err := f.e.persistAdvance(a, "completed", "capture_retained", "")
	if err != nil || recovered.ID != committed.ID || recovered.Revision != committed.Revision || attemptState(t, f, committed) != before {
		t.Fatal("exact committed transition was not reconciled without another write", err)
	}
	_, err = f.e.persistAdvance(a, "completed", "different_evidence", "")
	attemptCode(t, err, "review_persistence_failed")
	if attemptState(t, f, committed) != before {
		t.Fatal("same state with different semantic reason was incorrectly reconciled")
	}
	_, err = f.e.persistAdvance(a, "failed", "different_outcome", "")
	attemptCode(t, err, "review_persistence_failed")
	if attemptState(t, f, committed) != before {
		t.Fatal("incompatible CAS reconciliation changed durable outcome")
	}
}
