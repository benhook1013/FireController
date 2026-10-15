package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
)

func TestSupervisorPendingSkipsBusyBeforeLimitAndWakesAfterRelease(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	busy := f.start(t, attemptProject, f.enqueue(t, attemptProject, "reserved", "cli", 2, 1))
	seed := f.enqueue(t, attemptProject, "busy-seed", "cli", 4, 1)
	other := f.enqueue(t, otherAttemptProject, "other-account", "cli", 2, 1)
	hosted := f.enqueue(t, attemptProject, "other-channel", "hosted", 4, 1)
	// Exercise the selection bound independently of public admission's smaller
	// capacity, proving busy prefixes cannot hide later eligible work.
	for i := range 299 {
		if _, err := f.store.DB().Exec(`INSERT INTO review_operations SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint FROM review_operations WHERE id=?`, fmt.Sprintf("busy-%03d", i), seed.ID); err != nil {
			t.Fatal(err)
		}
	}
	before := attemptState(t, f, busy)
	operations, err := f.e.pending(context.Background())
	if err != nil || len(operations) != 2 {
		t.Fatalf("busy prefix hid eligible account/channel: count=%d err=%v", len(operations), err)
	}
	found := map[string]bool{}
	for _, o := range operations {
		found[o.ID] = true
	}
	if !found[other.ID] || !found[hosted.ID] || before != attemptState(t, f, busy) {
		t.Fatal("pending changed reservations or selected reserved work")
	}
	if _, err := f.e.Advance(context.Background(), attemptProject, busy.ID, busy.Revision, "cancelled", "synthetic_release", ""); err != nil {
		t.Fatal(err)
	}
	operations, err = f.e.pending(context.Background())
	if err != nil || len(operations) != 256 {
		t.Fatalf("released reservation did not make bounded queued work eligible: %d %v", len(operations), err)
	}
}

func TestSupervisorPendingUsesRetainedAccountAndStableTarget(t *testing.T) {
	f := supervisorFixture(t)
	f.start(t, attemptProject, f.enqueue(t, attemptProject, "reserved", "cli", 2, 1))
	f.enqueue(t, attemptProject, "original-account", "cli", 4, 1)
	f.configure(t, attemptProject, "other-account", 1, false)
	f.enqueue(t, attemptProject, "reserved-target-other-account", "cli", 2, 2)
	free := f.enqueue(t, attemptProject, "free-target-other-account", "cli", 4, 2)
	hosted := f.enqueue(t, attemptProject, "independent-channel", "hosted", 2, 2)
	if _, err := f.store.DB().Exec(`UPDATE projects SET repository='renamed/repository' WHERE id=?`, attemptProject); err != nil {
		t.Fatal(err)
	}
	operations, err := f.e.pending(context.Background())
	if err != nil || len(operations) != 2 {
		t.Fatalf("retained account/target filtering failed: %d %v", len(operations), err)
	}
	for _, o := range operations {
		if o.ID != free.ID && o.ID != hosted.ID {
			t.Fatal("selected reserved original account or stable target after rename")
		}
	}
	// Removed capabilities still reach StartAttempt's normal hold handling;
	// absence of a binding is not proof that a current account is occupied.
	withoutCapabilities, err := New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	operations, err = withoutCapabilities.pending(context.Background())
	if err != nil || len(operations) != 4 {
		t.Fatalf("unavailable capabilities were silently hidden: %d %v", len(operations), err)
	}
}

func TestSupervisorBusyQueueDoesNotEnterWriterAndCoordinationStaysUsable(t *testing.T) {
	f := supervisorFixture(t)
	if err := inbox.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	inbox.Register(f.handler.(*http.ServeMux), f.store)
	for _, worker := range []string{"sender", "recipient"} {
		if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,?)`, attemptProject, worker, worker); err != nil {
			t.Fatal(err)
		}
	}
	r := f.enqueue(t, attemptProject, "reserved", "cli", 2, 1)
	busy := f.start(t, attemptProject, r)
	if _, err := f.e.Admit(context.Background(), attemptProject, busy.ID, busy.Revision, attemptAnchor(attemptProject, 2), ""); err != nil {
		t.Fatal(err)
	}
	for i := range 31 {
		f.enqueue(t, attemptProject, fmt.Sprintf("busy-%02d", i), "cli", 4, 1)
	}
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	for i := range 8 {
		f.enqueue(t, otherAttemptProject, fmt.Sprintf("other-project-busy-%02d", i), "cli", 2, 1)
	}
	// Occupy the real writer admission slot. A read-only busy scan must finish
	// without waiting for it; a failed StartAttempt would still wait here.
	entered, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- f.store.BackgroundWrite(context.Background(), func(context.Context, *sql.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	driver := &controlledDriver{}
	go func() { done <- f.e.processNext(ctx, map[string]Driver{"test-capability": driver}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("reserved queue attempted writer admission")
		cancel()
		<-done
	}
	cancel()
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	driver.prepare = func(ctx context.Context, _ Operation, _ Configuration, _ Attempt) (Preparation, error) {
		ready <- struct{}{}
		<-ctx.Done()
		return Preparation{}, ctx.Err()
	}
	stop := runSupervisor(t, f, driver)
	waitOperationState(t, f, r.ID, "uncertain")
	started := time.Now()
	for i := range 5 {
		proveOrdinaryCoordination(t, f, fmt.Sprintf("busy-job-%d", i), r.ID)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+attemptProject+"/messages", strings.NewReader(`{"recipient_worker_id":"recipient","body":"Synthetic busy-queue coordination"}`))
		request.Header.Set("Authorization", "Bearer "+f.writer)
		request.Header.Set("X-FC-Worker", "sender")
		request.Header.Set("Idempotency-Key", fmt.Sprintf("busy-message-%d", i))
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("message write failed during busy scheduling: %d %s", response.Code, response.Body.String())
		}
	}
	t.Logf("five job/message write pairs with busy supervisor: %s", time.Since(started))
	if time.Since(started) > time.Second || driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
		t.Fatal("busy queue delayed coordination or performed provider work")
	}
	if _, err := f.e.Advance(context.Background(), attemptProject, busy.ID, busy.Revision+2, "cancelled", "synthetic_release", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not resume queued work after reservation release")
	}
	stop()
}

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

func TestSupervisorAttemptStartStorageFailureStopsWithoutDispatch(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "start-attempt-storage-failure", "cli", 2, 1)
	// The read-only queue selection must work. Fail the real admission INSERT,
	// where silently skipping every error would leave queued work retrying forever.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_attempt_insert BEFORE INSERT ON review_execution_attempts BEGIN SELECT RAISE(ABORT,'synthetic-private-storage-diagnostic'); END`); err != nil {
		t.Fatal(err)
	}
	pending, err := f.e.pending(context.Background())
	if err != nil || len(pending) != 1 || pending[0].ID != receipt.ID {
		t.Fatal("fixture did not isolate StartAttempt failure", pending, err)
	}
	beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	beforeReceipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	driver := &controlledDriver{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = f.e.Run(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
	attemptCode(t, err, "review_persistence_failed")
	if strings.Contains(err.Error(), "synthetic-private-storage-diagnostic") {
		t.Fatal("supervisor exposed private persistence diagnostic")
	}
	if ctx.Err() != nil || f.e.running.Load() || driver.prepares.Load() != 0 || driver.calls.Load() != 0 || supervisorOperationState(t, f, receipt.ID) != "queued" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != beforeReceipts {
		t.Fatal("attempt-start failure did not stop promptly/atomically before provider work")
	}
	var revision int64
	var attemptID string
	if err := f.store.DB().QueryRow(`SELECT revision,attempt_id FROM review_operations WHERE id=?`, receipt.ID).Scan(&revision, &attemptID); err != nil || revision != 1 || attemptID != "" {
		t.Fatal("failed attempt start changed queued identity", revision, attemptID, err)
	}
}

func TestSupervisorAdmissionStorageFailureRetainsPreparingAttempt(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "admission-storage-failure", "cli", 2, 1)
	// StartAttempt and pending reads remain healthy. Only the real preparing to
	// active admission statement fails, so a hold would falsely claim changed facts.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_admission BEFORE UPDATE OF state ON review_execution_attempts WHEN OLD.state='preparing' AND NEW.state='active' BEGIN SELECT RAISE(ABORT,'synthetic-private-admission-diagnostic'); END`); err != nil {
		t.Fatal(err)
	}
	driver := &controlledDriver{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := f.e.Run(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
	attemptCode(t, err, "review_persistence_failed")
	if strings.Contains(err.Error(), "synthetic-private-admission-diagnostic") {
		t.Fatal("admission infrastructure error exposed private diagnostics")
	}
	if ctx.Err() != nil || f.e.running.Load() || driver.prepares.Load() != 1 || driver.calls.Load() != 0 || supervisorOperationState(t, f, receipt.ID) != "preparing" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != 1 {
		t.Fatal("infrastructure failure was hidden as a hold or lost recoverable preparation")
	}
	var state, anchor, intent, reason string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT a.state,a.revision,a.anchor_json,a.intent,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE o.id=?`, receipt.ID).Scan(&state, &revision, &anchor, &intent, &reason); err != nil || state != "preparing" || revision != 1 || anchor != "null" || intent != "" || reason != "" {
		t.Fatal("failed admission partially changed original facts", state, revision, anchor, intent, reason, err)
	}
	// Explicit normal startup recovery can cancel undispatched preparation; this
	// failure path itself must not invent that later lifecycle decision.
	if err := f.e.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if supervisorOperationState(t, f, receipt.ID) != "cancelled" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || driver.calls.Load() != 0 {
		t.Fatal("retained preparation was not safely recoverable")
	}
}

func TestSupervisorRetainedConfigurationFailureRetainsPreparingAttempt(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "retained-configuration-failure", "cli", 2, 1)
	// StartAttempt validates the configuration before inserting its attempt.
	// Corrupt that exact retained revision only after validation, so the next
	// configuration read fails after preparation and its reservation commit.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER corrupt_retained_configuration AFTER INSERT ON review_execution_attempts BEGIN UPDATE review_execution_config SET body_json='synthetic-private-configuration-diagnostic' WHERE project_id=NEW.project_id AND revision=(SELECT configuration_revision FROM review_operations WHERE id=NEW.operation_id); END`); err != nil {
		t.Fatal(err)
	}
	beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	beforeReceipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	driver := &controlledDriver{}
	err := f.e.processNext(context.Background(), map[string]Driver{"test-capability": driver, "other-account": driver})
	attemptCode(t, err, "review_persistence_failed")
	if err.Error() != persistenceFailure().Error() {
		t.Fatal("retained configuration failure exposed private diagnostics", err)
	}
	var attemptID, operationState, state, anchor, intent, reason string
	var operationRevision, revision int64
	if err := f.store.DB().QueryRow(`SELECT a.id,o.state,o.revision,a.state,a.revision,a.anchor_json,a.intent,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE o.id=?`, receipt.ID).Scan(&attemptID, &operationState, &operationRevision, &state, &revision, &anchor, &intent, &reason); err != nil || attemptID == "" || operationState != "preparing" || operationRevision != 2 || state != "preparing" || revision != 1 || anchor != "null" || intent != "" || reason != "" {
		t.Fatal("configuration read failure changed original preparation", operationState, operationRevision, state, revision, anchor, intent, reason, err)
	}
	var reservedAttempt, transitionState, transitionReason string
	var transitionRevision int64
	if err := f.store.DB().QueryRow(`SELECT attempt_id FROM review_execution_reservations`).Scan(&reservedAttempt); err != nil || reservedAttempt != attemptID {
		t.Fatal("configuration read failure lost original reservation", reservedAttempt, err)
	}
	if err := f.store.DB().QueryRow(`SELECT revision,state,reason FROM review_execution_transitions WHERE attempt_id=?`, attemptID).Scan(&transitionRevision, &transitionState, &transitionReason); err != nil || transitionRevision != 1 || transitionState != "preparing" || transitionReason != "preparing" {
		t.Fatal("configuration read failure rewrote original transition", transitionRevision, transitionState, transitionReason, err)
	}
	if driver.prepares.Load() != 0 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != beforeReceipts {
		t.Fatal("configuration failure dispatched, settled or added journal effects")
	}
}

func TestSupervisorShutdownAfterSuccessfulPrepareCancelsBeforeAdmission(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "shutdown-before-admission", "cli", 2, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		anchor := attemptAnchor(o.ProjectID, o.PullNumber)
		cancel()
		return Preparation{Anchor: anchor}, nil
	}}
	if err := f.e.Run(ctx, map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
		t.Fatal("shutdown after successful preflight failed", err)
	}
	var state, reason, anchor, intent string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT a.state,a.revision,a.anchor_json,a.intent,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE o.id=?`, receipt.ID).Scan(&state, &revision, &anchor, &intent, &reason); err != nil || state != "cancelled" || revision != 2 || anchor != "null" || intent != "" || reason != "supervisor_stopping" || driver.prepares.Load() != 1 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("undispatched shutdown was incorrectly held or admitted", state, reason, revision, err)
	}
}

func TestSupervisorAdmissionValidationRefusalRemainsHeld(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "invalid-prepared-anchor", "cli", 2, 1)
	driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		anchor := attemptAnchor(o.ProjectID, o.PullNumber)
		anchor.CandidateSHA = "invalid"
		return Preparation{Anchor: anchor}, nil
	}}
	if err := f.e.processNext(context.Background(), map[string]Driver{"test-capability": driver, "other-account": driver}); err != nil {
		t.Fatal("admission refusal became infrastructure failure", err)
	}
	var state, reason string
	if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_operations WHERE id=?`, receipt.ID).Scan(&state, &reason); err != nil || state != "held" || reason != "admission_changed" || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("invalid admission contract was not safely held", state, reason, err)
	}
}

func TestSupervisorAdmissionWriterOverloadRetainsPreparation(t *testing.T) {
	f := supervisorFixture(t)
	receipt := f.enqueue(t, attemptProject, "admission-writer-overload", "cli", 2, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	// Hold the real serialized writer after StartAttempt commits. The admission
	// call must exhaust its own finite writer budget, independently of shutdown.
	driver := &controlledDriver{prepare: func(_ context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		go func() {
			writerDone <- f.store.BackgroundWrite(context.Background(), func(context.Context, *sql.Tx) error { close(entered); <-release; return nil })
		}()
		<-entered
		return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
	}}
	released := false
	defer func() {
		if !released {
			close(release)
			<-writerDone
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	err := f.e.Run(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
	close(release)
	released = true
	<-writerDone // Only this fixture's held transaction; it has no domain writes.
	attemptCode(t, err, "review_persistence_failed")
	if ctx.Err() != nil || f.e.running.Load() || supervisorOperationState(t, f, receipt.ID) != "preparing" || driver.prepares.Load() != 1 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != 1 {
		t.Fatal("writer overload was hidden as changed admission facts or retried externally")
	}
}
