package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSupervisorPendingReadPoolExhaustionPreservesQueuedWork(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "pending-read-contention", "cli", 2, 1)
	driver := &controlledDriver{}
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver})
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal("read-only pool contention stopped supervisor", err)
	}
	if elapsed := time.Since(started); elapsed < time.Second || elapsed > 4*time.Second {
		t.Fatal("read contention was not bounded", elapsed)
	}
	if driver.prepares.Load() != 0 || driver.calls.Load() != 0 || supervisorOperationState(t, f, r.ID) != "queued" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("read-only contention changed admission or started provider work")
	}
	// The next ordinary worker pass uses the original request, not a replacement.
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
		t.Fatal(err)
	}
	if driver.prepares.Load() != 1 || driver.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 {
		t.Fatal("released read pool did not resume original queued work exactly once")
	}
}

func TestSupervisorCooldownAdmissionContentionKeepsOriginalTimerAndSibling(t *testing.T) {
	f := supervisorFixture(t)
	cliEntered, cliRelease := make(chan struct{}), make(chan struct{})
	cliCancelled := make(chan struct{})
	writerEntered, writerRelease := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	hostedReady := make(chan Attempt, 1)
	prepareFailure := make(chan error, 1)
	var queries atomic.Int32
	driver := &controlledDriver{}
	driver.prepare = func(ctx context.Context, o Operation, _ Configuration, a Attempt) (prepared Preparation, failure error) {
		defer func() {
			if o.Channel == "hosted" && failure != nil {
				prepareFailure <- failure
			}
		}()
		if o.Channel == "cli" {
			return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
		}
		<-cliEntered
		anchor := attemptAnchor(o.ProjectID, o.PullNumber)
		q, _, err := f.e.BeginHostedAllowanceQuery(ctx, o.ProjectID, a.ID, a.Revision, anchor, reviewprovider.Author{ID: 12, Login: "poster", Kind: "User"}, reviewprovider.Author{ID: 30, Login: "coderabbitai[bot]", Kind: "Bot"})
		if err != nil {
			return Preparation{}, err
		}
		queries.Add(1)
		q, err = f.e.ConfirmHostedAllowanceQuery(ctx, o.ProjectID, a.ID, a.Revision, hostedQueryComment(t, q))
		if err != nil {
			return Preparation{}, err
		}
		observed := time.Now().UTC()
		digest := strings.Repeat("c", 64)
		start, _ := time.Parse(time.RFC3339Nano, q.EarliestPosting)
		in := HostedAllowanceInput{QueryID: q.ID, Anchor: anchor, ObservedAt: observed.Format(time.RFC3339Nano), ChangedFiles: 2, CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1234, Observation: reviewprovider.HostedAllowanceObservation{State: "cooldown_observed", Reason: "provider_cooldown_observed", QueryCommentID: q.Comment.ID, ResponseCommentID: 92, QueryCreatedAt: q.Comment.CreatedAt, ResponseCreatedAt: q.Comment.CreatedAt.Add(time.Nanosecond), RetryNotBefore: q.Comment.CreatedAt.Add(time.Nanosecond).Add(3 * time.Minute)}}
		_, err = f.e.RecordHostedObservation(ctx, o.ProjectID, a.ID, HostedObservationInput{Kind: "allowance_query", CaptureID: in.CaptureID, CaptureSHA256: digest, CaptureBytes: in.CaptureBytes, StartedAt: start.Truncate(time.Second).Format(time.RFC3339Nano), FinishedAt: in.ObservedAt, ObservedAt: in.ObservedAt, ExpectedAnchor: anchor, Intent: q.Intent})
		if err == nil {
			err = f.e.RecordHostedCooldown(ctx, o.ProjectID, a.ID, a.Revision, in)
		}
		if err != nil {
			return Preparation{}, err
		}
		go func() {
			writerDone <- f.store.BackgroundWrite(context.Background(), func(context.Context, *sql.Tx) error { close(writerEntered); <-writerRelease; return nil })
		}()
		<-writerEntered
		hostedReady <- a
		return Preparation{}, &core.Error{Code: "review_held", Details: map[string]string{"reason": "hosted_allowance_cooldown"}}
	}
	driver.dispatch = func(ctx context.Context, o Operation, _ Attempt, _ func(context.Context, string) error) (Result, error) {
		if o.Channel != "cli" {
			return Result{}, errors.New("cooldown dispatched a review")
		}
		close(cliEntered)
		select {
		case <-cliRelease:
			return Result{State: "completed", Reason: "synthetic_capture"}, nil
		case <-ctx.Done():
			close(cliCancelled)
			return Result{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.Start(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-writerRelease:
		default:
			close(writerRelease)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("supervisor leaked")
		}
	})
	cli := f.enqueue(t, attemptProject, "cooldown-sibling", "cli", 2, 1)
	select {
	case <-cliEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("CLI sibling did not dispatch")
	}
	hosted := f.enqueue(t, attemptProject, "contended-original-cooldown", "hosted", 2, 1)
	var a Attempt
	select {
	case a = <-hostedReady:
	case <-time.After(3 * time.Second):
		select {
		case err := <-prepareFailure:
			t.Fatal("Hosted preflight failed", err)
		default:
			t.Fatal("Hosted preflight did not retain cooldown", supervisorOperationState(t, f, hosted.ID))
		}
	}
	// Hold the real writer beyond BackgroundWrite's 2s admission deadline.
	select {
	case err := <-done:
		close(writerRelease)
		<-writerDone
		t.Fatal("refused cooldown admission killed supervisor", err)
	case <-time.After(2250 * time.Millisecond):
	}
	select {
	case <-cliCancelled:
		close(writerRelease)
		<-writerDone
		t.Fatal("cooldown admission contention cancelled independent CLI")
	default:
	}
	close(writerRelease)
	if err := <-writerDone; err != nil {
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "overloaded" {
			t.Fatal(err)
		}
	}
	waitSupervisor(t, "original cooldown timer", func() bool {
		var state, id string
		_ = f.store.DB().QueryRow(`SELECT state,attempt_id FROM review_operations WHERE id=?`, hosted.ID).Scan(&state, &id)
		return state == "queued" && id == ""
	})
	if supervisorOperationState(t, f, cli.ID) != "active" || queries.Load() != 1 || driver.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_allowance_queries`) != 1 {
		t.Fatal("cooldown retried provider work or cancelled CLI")
	}
	var baseline string
	if err := f.store.DB().QueryRow(`SELECT baseline_attempt_id FROM review_operation_waits WHERE operation_id=?`, hosted.ID).Scan(&baseline); err != nil || baseline != a.ID {
		t.Fatal("timer lost original attempt", baseline, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, a.ID) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("cooldown did not settle exactly once")
	}
	close(cliRelease)
	waitOperationState(t, f, cli.ID, "completed")
}

func TestSupervisorCooldownEnteredFailureIsFatalAndExactReplayReconciles(t *testing.T) {
	f, a, in := cooldownObservationFixture(t)
	if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	o := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
	o.Revision-- // processNext retains the queued revision before StartAttempt.
	var entered atomic.Int32
	f.e.publishAttempt = func(context.Context, *sql.Tx, Attempt) error {
		entered.Add(1)
		return core.Fail("overloaded", "Private callback failure", 503)
	}
	_, err := f.e.persistHostedCooldown(t.Context(), o, a)
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_persistence_failed" || failure.Details.(map[string]string)["stage"] != "cooldown_schedule" || entered.Load() != 1 {
		t.Fatal("entered callback error was retried or hidden", err, entered.Load())
	}
	if supervisorOperationState(t, f, a.OperationID) != "preparing" || attemptCount(t, f.store, `SELECT count(*) FROM review_operation_waits`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
		t.Fatal("entered failure lost original uncertainty")
	}
	f.e.publishAttempt = nil
	committed, err := f.e.ScheduleHostedCooldown(t.Context(), a.ProjectID, a.ID, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTable(t, f, "review_execution_transitions") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "events")
	replayed, err := f.e.persistHostedCooldown(t.Context(), o, a)
	if err != nil || replayed.ID != committed.ID || replayed.Revision != committed.Revision || before != snapshotTable(t, f, "review_execution_transitions")+snapshotTable(t, f, "review_operation_waits")+snapshotTable(t, f, "events") {
		t.Fatal("exact committed timer was not reconciled without mutation", err)
	}
	o.Revision++
	if _, err := f.e.persistHostedCooldown(t.Context(), o, a); err == nil {
		t.Fatal("wrong original CAS accepted")
	}
}

func TestSupervisorPersistenceDiagnosticsKeepSafeStageAndCause(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "safe-fatal-diagnostic", "cli", 2, 1)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_diagnostic_attempt BEFORE INSERT ON review_execution_attempts BEGIN SELECT RAISE(ABORT,'private SQL credential text'); END`); err != nil {
		t.Fatal(err)
	}
	driver := &controlledDriver{}
	err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver, "other-account": driver})
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_persistence_failed" {
		t.Fatal(err)
	}
	details, ok := failure.Details.(map[string]string)
	if !ok || details["stage"] != "attempt_start" || details["cause"] != "storage_unavailable" || details["operation_id"] != r.ID || details["attempt_id"] != "" || len(details) != 3 {
		t.Fatal("wrong safe original failure facts", details)
	}
	body, _ := json.Marshal(failure)
	if strings.Contains(string(body), "private SQL") || driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
		t.Fatal("storage failure leaked or dispatched")
	}
}

func TestSupervisorCorruptPendingReadRemainsFatal(t *testing.T) {
	f := supervisorFixture(t)
	if _, err := f.store.DB().Exec(`ALTER TABLE review_operations RENAME TO broken_review_operations`); err != nil {
		t.Fatal(err)
	}
	err := f.e.processNext(t.Context(), map[string]Driver{})
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_persistence_failed" {
		t.Fatal("corrupt storage was treated as contention", err)
	}
	details := failure.Details.(map[string]string)
	if details["stage"] != "pending_read" || details["cause"] != "storage_unavailable" || len(details) != 2 {
		t.Fatal(details)
	}
}

func TestSupervisorOutcomeDiagnosticsPreserveGuardedCAS(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "outcome-diagnostic", "cli", 2, 1))
	_, err := f.e.persistAdvance(a, "completed", "invalid_from_preparing", "")
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_persistence_failed" {
		t.Fatal(err)
	}
	details := failure.Details.(map[string]string)
	if details["stage"] != "outcome_write" || details["cause"] != "review_conflict" || details["operation_id"] != a.OperationID || details["attempt_id"] != a.ID {
		t.Fatal(details)
	}
	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&state); err != nil || state != "preparing" {
		t.Fatal("diagnostic changed guarded attempt", state, err)
	}
	unknown := persistenceFailureAt("outcome_write", errors.New("private underlying error"), "private bad ID", "private bad ID").(*core.Error)
	data, _ := json.Marshal(unknown)
	if strings.Contains(string(data), "private") || unknown.Details.(map[string]string)["cause"] != "storage_unavailable" {
		t.Fatal(string(data))
	}
}
