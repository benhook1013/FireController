package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
)

type controlledDriver struct {
	prepare  func(context.Context, Operation, Configuration, Attempt) (Preparation, error)
	dispatch func(context.Context, Operation, Attempt, func(context.Context, string) error) (Result, error)
	prepares atomic.Int32
	calls    atomic.Int32
}

func (d *controlledDriver) Prepare(ctx context.Context, o Operation, c Configuration, a Attempt) (Preparation, error) {
	d.prepares.Add(1)
	if d.prepare != nil {
		return d.prepare(ctx, o, c, a)
	}
	intent := ""
	if o.Channel == "hosted" {
		intent = "synthetic-intent-" + a.ID
	}
	return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber), Intent: intent}, nil
}

func (d *controlledDriver) Dispatch(ctx context.Context, o Operation, a Attempt, trigger func(context.Context, string) error) (Result, error) {
	d.calls.Add(1)
	if d.dispatch != nil {
		return d.dispatch(ctx, o, a, trigger)
	}
	return Result{State: "completed", Reason: "synthetic_capture"}, nil
}

func supervisorFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	if err := jobs.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	jobs.Register(mux, f.store)
	f.handler = mux
	f.configure(t, attemptProject, "test-capability", 0, false)
	return f
}

func waitSupervisor(t *testing.T, message string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out: " + message)
}

func runSupervisor(t *testing.T, f *attemptFixture, driver Driver) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.e.Run(ctx, map[string]Driver{"test-capability": driver, "other-account": driver}) }()
	waitSupervisor(t, "supervisor ownership", func() bool { return f.e.running.Load() })
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("orderly supervisor shutdown:", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("cooperative supervisor did not shut down in finite time")
		}
	}
	t.Cleanup(stop)
	return stop
}

func supervisorOperationState(t *testing.T, f *attemptFixture, id string) string {
	t.Helper()
	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM review_operations WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func waitOperationState(t *testing.T, f *attemptFixture, id, state string) {
	t.Helper()
	waitSupervisor(t, "operation state "+state, func() bool { return supervisorOperationState(t, f, id) == state })
}

func supervisorRequest(t *testing.T, f *attemptFixture, key, channel string, number int) (Receipt, []byte) {
	t.Helper()
	data, _ := json.Marshal(Request{ExpectedConfigurationRevision: 1, PullNumber: int64(number), Channel: channel, HeadSHA: attemptAnchor(attemptProject, int64(number)).CandidateSHA})
	started := time.Now()
	response := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/requests", f.writer, key, string(data))
	if time.Since(started) > time.Second || response.Code != http.StatusAccepted {
		t.Fatalf("receipt waited for provider or failed: %d %s", response.Code, response.Body.String())
	}
	var receipt Receipt
	var fields map[string]any
	if json.Unmarshal(response.Body.Bytes(), &receipt) != nil || json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 3 {
		t.Fatal("receipt was not compact id/state/revision")
	}
	return receipt, append([]byte(nil), response.Body.Bytes()...)
}

func proveOrdinaryCoordination(t *testing.T, f *attemptFixture, key, operation string) {
	t.Helper()
	started := time.Now()
	created := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/jobs", f.writer, key, `{"title":"Synthetic coordination during provider wait","brief":"Private fixture","status":"active","worker_id":""}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("ordinary write blocked/failed: %d %s", created.Code, created.Body.String())
	}
	listed := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/jobs", f.reader, "", "")
	read := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/operations/"+operation, f.reader, "", "")
	if listed.Code != http.StatusOK || read.Code != http.StatusOK || time.Since(started) > time.Second {
		t.Fatal("ordinary authenticated read/write waited for provider")
	}
}

func TestSupervisorReceiptsAndCoordinationRemainFastDuringProviderWait(t *testing.T) {
	f := supervisorFixture(t)
	prepareEntered, dispatchEntered := make(chan struct{}), make(chan struct{})
	releasePreparation, releaseDispatch := make(chan struct{}), make(chan struct{})
	driver := &controlledDriver{}
	driver.prepare = func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		if o.PullNumber != 2 {
			<-ctx.Done()
			return Preparation{}, ctx.Err()
		}
		close(prepareEntered)
		select {
		case <-releasePreparation:
			return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
		case <-ctx.Done():
			return Preparation{}, ctx.Err()
		}
	}
	driver.dispatch = func(ctx context.Context, _ Operation, _ Attempt, _ func(context.Context, string) error) (Result, error) {
		close(dispatchEntered)
		select {
		case <-releaseDispatch:
			return Result{State: "completed", Reason: "process_exit_zero"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	stop := runSupervisor(t, f, driver)
	receipt, original := supervisorRequest(t, f, "fast-receipt", "cli", 2)
	select {
	case <-prepareEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("preparation did not begin")
	}
	proveOrdinaryCoordination(t, f, "write-during-preparation", receipt.ID)
	queuedDuringPrepare, _ := supervisorRequest(t, f, "new-during-preparation", "cli", 4)
	if supervisorOperationState(t, f, queuedDuringPrepare.ID) != "queued" {
		t.Fatal("new receipt waited for or bypassed occupied account during preparation")
	}
	_, replay := supervisorRequest(t, f, "fast-receipt", "cli", 2)
	if !bytes.Equal(original, replay) || driver.prepares.Load() != 1 {
		t.Fatal("receipt replay re-enqueued preparation")
	}
	close(releasePreparation)
	select {
	case <-dispatchEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not begin")
	}
	proveOrdinaryCoordination(t, f, "write-during-dispatch", receipt.ID)
	queuedDuringDispatch, _ := supervisorRequest(t, f, "new-during-dispatch", "cli", 4)
	if supervisorOperationState(t, f, queuedDuringDispatch.ID) != "queued" {
		t.Fatal("new receipt waited for or bypassed occupied account during dispatch")
	}
	_, replay = supervisorRequest(t, f, "fast-receipt", "cli", 2)
	if !bytes.Equal(original, replay) || driver.calls.Load() != 1 {
		t.Fatal("replay dispatched a duplicate")
	}
	close(releaseDispatch)
	waitOperationState(t, f, receipt.ID, "completed")
	stop()
	for _, table := range []string{"review_workspace", "review_snapshots", "review_runs", "review_findings"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("driver exit/state manufactured review credit")
		}
	}
}

func TestSupervisorTwoSlotsSkipBusyAccountAndKeepChannelsIndependent(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	entered := make(chan Operation, 8)
	releaseHosted := make(chan struct{})
	var active, peak atomic.Int32
	driver := &controlledDriver{dispatch: func(ctx context.Context, o Operation, _ Attempt, _ func(context.Context, string) error) (Result, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for n > peak.Load() {
			previous := peak.Load()
			if peak.CompareAndSwap(previous, n) {
				break
			}
		}
		entered <- o
		if o.Channel == "hosted" {
			select {
			case <-releaseHosted:
				return Result{State: "failed", Reason: "synthetic_no_post"}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	stop := runSupervisor(t, f, driver)
	f.enqueue(t, attemptProject, "slot-one", "cli", 2, 1)
	select {
	case o := <-entered:
		if o.Channel != "cli" || o.PullNumber != 2 {
			t.Fatal("wrong first dispatch")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first slot did not dispatch")
	}
	busy := f.enqueue(t, attemptProject, "same-account-busy", "cli", 4, 1)
	f.enqueue(t, attemptProject, "independent-hosted", "hosted", 2, 1)
	select {
	case o := <-entered:
		if o.Channel != "hosted" {
			t.Fatal("busy CLI account was not skipped")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("independent hosted channel was blocked")
	}
	third := f.enqueue(t, otherAttemptProject, "third-free-account", "cli", 2, 1)
	select {
	case <-entered:
		t.Fatal("more than two driver slots became active")
	case <-time.After(150 * time.Millisecond):
	}
	if supervisorOperationState(t, f, busy.ID) != "queued" || supervisorOperationState(t, f, third.ID) != "queued" {
		t.Fatal("busy or third request left queue before its slot was available")
	}
	close(releaseHosted)
	select {
	case o := <-entered:
		if o.ProjectID != otherAttemptProject {
			t.Fatal("supervisor failed to skip independently busy account")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("released slot did not serve another account")
	}
	stop()
	if peak.Load() != 2 || active.Load() != 0 {
		t.Fatal("global slot bound or cooperative cleanup failed")
	}
}

func TestSupervisorPreparationChangesAndFailuresNeverDispatch(t *testing.T) {
	for _, change := range []string{"configuration", "revocation", "preflight_error"} {
		t.Run(change, func(t *testing.T) {
			f := supervisorFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
				close(entered)
				select {
				case <-ctx.Done():
					return Preparation{}, ctx.Err()
				case <-release:
				}
				if change == "preflight_error" {
					return Preparation{}, errors.New("private provider diagnostic")
				}
				return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
			}}
			stop := runSupervisor(t, f, driver)
			r := f.enqueue(t, attemptProject, "prepare-"+change, "cli", 2, 1)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("preflight did not start")
			}
			if change == "configuration" {
				f.configure(t, attemptProject, "test-capability", 1, false)
			}
			if change == "revocation" {
				r := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/"+attemptProject+"/credentials/exec-writer-cred", strings.NewReader(`{}`))
				r.Header.Set("Authorization", "Bearer "+f.admin)
				r.Header.Set("Idempotency-Key", "revoke-during-preflight")
				response := httptest.NewRecorder()
				server.New(f.store).ServeHTTP(response, r)
				if response.Code != http.StatusOK {
					t.Fatalf("credential revocation: %d %s", response.Code, response.Body.String())
				}
			}
			close(release)
			waitOperationState(t, f, r.ID, "held")
			stop()
			if driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("preflight hold dispatched or retained reservation")
			}
			var reason string
			if err := f.store.DB().QueryRow(`SELECT reason FROM review_operations WHERE id=?`, r.ID).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if reason != "admission_changed" && reason != "preparation_failed" {
				t.Fatal("unsafe preflight reason", reason)
			}
		})
	}
}

func TestSupervisorHostedTriggerPersistsDuringDispatch(t *testing.T) {
	f := supervisorFixture(t)
	callbackResults := make(chan []error, 1)
	release := make(chan struct{})
	driver := &controlledDriver{dispatch: func(ctx context.Context, _ Operation, _ Attempt, record func(context.Context, string) error) (Result, error) {
		callbackResults <- []error{record(ctx, "comment_123"), record(ctx, "comment_123"), record(ctx, "comment_456")}
		select {
		case <-release:
			return Result{State: "completed", Reason: "capture_retained", ExternalID: "comment_123"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	stop := runSupervisor(t, f, driver)
	r := f.enqueue(t, attemptProject, "hosted-trigger", "hosted", 2, 1)
	select {
	case results := <-callbackResults:
		if results[0] != nil || results[1] != nil {
			t.Fatal("valid/repeated trigger failed", results)
		}
		attemptCode(t, results[2], "review_conflict")
	case <-time.After(3 * time.Second):
		t.Fatal("hosted trigger callback did not run")
	}
	waitOperationState(t, f, r.ID, "active")
	var id, intent, external, anchor string
	var revision int
	if err := f.store.DB().QueryRow(`SELECT id,intent,external_id,anchor_json,revision FROM review_execution_attempts WHERE operation_id=?`, r.ID).Scan(&id, &intent, &external, &anchor, &revision); err != nil {
		t.Fatal(err)
	}
	if intent != "synthetic-intent-"+id || external != "comment_123" || revision != 3 || anchor == "null" {
		t.Fatal("immediate callback failed to retain trigger, anchor or intent")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, id) != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, id) != 1 {
		t.Fatal("repeated callback mutated journal or released active reservation")
	}
	proveOrdinaryCoordination(t, f, "coordination-while-trigger-waits", r.ID)
	close(release)
	waitOperationState(t, f, r.ID, "completed")
	stop()
}

func TestSupervisorDispatchErrorsAndInvalidResultsBecomeUncertain(t *testing.T) {
	for _, mode := range []string{"error", "invalid_state", "invalid_reason", "invalid_external_id", "changed_external_id"} {
		t.Run(mode, func(t *testing.T) {
			f := supervisorFixture(t)
			driver := &controlledDriver{dispatch: func(ctx context.Context, _ Operation, _ Attempt, record func(context.Context, string) error) (Result, error) {
				switch mode {
				case "error":
					return Result{}, errors.New("private provider failure")
				case "invalid_state":
					return Result{State: "invented", Reason: "bad_result"}, nil
				case "invalid_reason":
					return Result{State: "completed", Reason: "bad reason"}, nil
				case "invalid_external_id":
					return Result{State: "completed", Reason: "retained", ExternalID: "bad/external"}, nil
				default:
					if err := record(ctx, "comment_123"); err != nil {
						return Result{}, err
					}
					return Result{State: "completed", Reason: "retained", ExternalID: "comment_456"}, nil
				}
			}}
			stop := runSupervisor(t, f, driver)
			channel := "cli"
			if mode == "invalid_external_id" || mode == "changed_external_id" {
				channel = "hosted"
			}
			r := f.enqueue(t, attemptProject, "uncertain-"+mode, channel, 2, 1)
			waitOperationState(t, f, r.ID, "uncertain")
			stop()
			if driver.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
				t.Fatal("uncertain action retried or lost reservation")
			}
			// Starting another supervisor cannot redispatch a settled uncertain journal.
			stopAgain := runSupervisor(t, f, driver)
			time.Sleep(40 * time.Millisecond)
			stopAgain()
			if driver.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 {
				t.Fatal("uncertainty caused replacement dispatch")
			}
		})
	}
}

func TestSupervisorCooperativeShutdownRetainsInterruptedDispatch(t *testing.T) {
	for _, phase := range []string{"prepare", "dispatch"} {
		t.Run(phase, func(t *testing.T) {
			f := supervisorFixture(t)
			entered := make(chan struct{})
			driver := &controlledDriver{}
			if phase == "prepare" {
				driver.prepare = func(ctx context.Context, _ Operation, _ Configuration, _ Attempt) (Preparation, error) {
					close(entered)
					<-ctx.Done()
					return Preparation{}, ctx.Err()
				}
			} else {
				driver.dispatch = func(ctx context.Context, _ Operation, _ Attempt, _ func(context.Context, string) error) (Result, error) {
					close(entered)
					<-ctx.Done()
					return Result{}, ctx.Err()
				}
			}
			stop := runSupervisor(t, f, driver)
			r := f.enqueue(t, attemptProject, "shutdown-"+phase, "cli", 2, 1)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("driver did not start")
			}
			started := time.Now()
			stop()
			if time.Since(started) > time.Second {
				t.Fatal("cooperative shutdown was not prompt")
			}
			state, reservations := "uncertain", 1
			if phase == "prepare" {
				state, reservations = "cancelled", 0
			}
			if supervisorOperationState(t, f, r.ID) != state || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != reservations {
				t.Fatal("shutdown lost dispatch ambiguity or retained undispatched work")
			}
		})
	}
}

func TestSupervisorRejectsInvalidDriverMapsAndDuplicateOwner(t *testing.T) {
	f := supervisorFixture(t)
	driver := &controlledDriver{}
	for _, drivers := range []map[string]Driver{nil, {"test-capability": driver}, {"test-capability": driver, "other-account": nil}, {"test-capability": driver, "other-account": driver, "unknown": driver}} {
		attemptCode(t, f.e.Run(context.Background(), drivers), "validation_failed")
		if f.e.running.Load() {
			t.Fatal("rejected supervisor retained ownership")
		}
	}
	stop := runSupervisor(t, f, driver)
	attemptCode(t, f.e.Run(context.Background(), map[string]Driver{"test-capability": driver, "other-account": driver}), "review_conflict")
	stop()
}

func TestSupervisorBackgroundWriteCancelsRealSQLiteAndRollsBackJournal(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "rollback-expensive-sql", "cli", 2, 1)
	beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	entered := false
	err := f.store.BackgroundWrite(ctx, func(derived context.Context, tx *sql.Tx) error {
		entered = true
		if _, ok := derived.Deadline(); !ok {
			t.Error("background transaction omitted deadline")
		}
		if _, err := tx.ExecContext(derived, `UPDATE review_operations SET reason='staged_private_reason' WHERE id=?`, r.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(derived, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) VALUES(?,'synthetic.staged',?,1,'synthetic','{}',?)`, attemptProject, r.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		var sum int64
		return tx.QueryRowContext(derived, `WITH RECURSIVE expensive(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM expensive WHERE n<1000000000) SELECT sum(n) FROM expensive`).Scan(&sum)
	})
	if !entered || time.Since(started) > time.Second {
		t.Fatal("expensive SQLite cancellation did not stay bounded")
	}
	attemptCode(t, err, "overloaded")
	var reason string
	if err := f.store.DB().QueryRow(`SELECT reason FROM review_operations WHERE id=?`, r.ID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "" || attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents {
		t.Fatal("cancelled transaction committed staged journal effects")
	}
	proveOrdinaryCoordination(t, f, "write-after-sql-cancel", r.ID)
}
