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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
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
		if _, err := f.store.DB().Exec(`INSERT INTO review_operations (id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations WHERE id=?`, fmt.Sprintf("busy-%03d", i), seed.ID); err != nil {
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
	done, err := f.e.Start(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
	if err != nil {
		cancel()
		t.Fatal("supervisor startup:", err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err, ok := <-done:
			if !ok || err != nil {
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
	var nilDriver *controlledDriver
	for _, drivers := range []map[string]Driver{nil, {"test-capability": driver}, {"test-capability": driver, "other-account": nil}, {"test-capability": driver, "other-account": nilDriver}, {"test-capability": driver, "other-account": driver, "unknown": driver}} {
		done, err := f.e.Start(context.Background(), drivers)
		attemptCode(t, err, "validation_failed")
		if done != nil {
			t.Fatal("invalid bindings returned a worker completion channel")
		}
		if f.e.running.Load() {
			t.Fatal("rejected supervisor retained ownership")
		}
	}
	stop := runSupervisor(t, f, driver)
	done, err := f.e.Start(context.Background(), map[string]Driver{"test-capability": driver, "other-account": driver})
	attemptCode(t, err, "review_conflict")
	if done != nil {
		t.Fatal("duplicate supervisor returned a worker completion channel")
	}
	stop()
}

func requireSupervisorCompletion(t *testing.T, f *attemptFixture, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err, ok := <-done:
		if !ok || err != nil {
			t.Fatal("missing or unsuccessful terminal result", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor shutdown did not complete")
	}
	select {
	case _, ok := <-done:
		if ok {
			t.Fatal("supervisor emitted more than one terminal result")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not close its completion channel")
	}
	if f.e.running.Load() {
		t.Fatal("terminal result was visible before ownership was released")
	}
}

func TestSupervisorStartCommitsRecoveryBeforeReturning(t *testing.T) {
	for _, state := range []string{"preparing", "posting", "active"} {
		t.Run(state, func(t *testing.T) {
			f := supervisorFixture(t)
			channel := "cli"
			if state == "posting" {
				channel = "hosted"
			}
			r := f.enqueue(t, attemptProject, "interrupted-before-start", channel, 2, 1)
			a := f.start(t, attemptProject, r)
			if state != "preparing" {
				intent := ""
				if channel == "hosted" {
					intent = "retained-startup-intent"
				}
				var err error
				a, err = f.e.Admit(context.Background(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), intent)
				if err != nil {
					t.Fatal(err)
				}
			}
			beforeTransitions := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, a.ID)
			driver := &controlledDriver{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done, err := f.e.Start(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
			if err != nil {
				t.Fatal(err)
			}
			// No readiness poll: the durable state must already be committed when
			// Start returns, before an HTTP listener would accept a new request.
			want, reservations := "uncertain", 1
			if state == "preparing" {
				want, reservations = "cancelled", 0
			}
			if supervisorOperationState(t, f, r.ID) != want || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != reservations || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, a.ID) != beforeTransitions+1 || driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
				t.Fatal("Start returned before recovery or dispatched interrupted work")
			}
			requireSupervisorCompletion(t, f, cancel, done)
		})
	}
}

func TestSupervisorStartRecoveryFailureHasNoWorkersAndAllowsRetry(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "startup-recovery-fault", "cli", 2, 1)
	a := f.start(t, attemptProject, r)
	before := attemptState(t, f, a)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_startup_recovery BEFORE UPDATE ON review_operations BEGIN SELECT RAISE(ABORT,'synthetic startup recovery fault'); END`); err != nil {
		t.Fatal(err)
	}
	driver := &controlledDriver{}
	drivers := map[string]Driver{"test-capability": driver, "other-account": driver}
	done, err := f.e.Start(context.Background(), drivers)
	if err == nil || done != nil || f.e.running.Load() || attemptState(t, f, a) != before || driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
		t.Fatal("failed synchronous recovery leaked ownership/workers or partial state", err)
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER reject_startup_recovery`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, err = f.e.Start(ctx, drivers)
	if err != nil || supervisorOperationState(t, f, r.ID) != "cancelled" {
		t.Fatal("valid startup could not retry recovery", err)
	}
	requireSupervisorCompletion(t, f, cancel, done)
}

func TestSupervisorStartWaitsForRecoveryTransaction(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "recovery-behind-owned-write", "cli", 2, 1)
	f.start(t, attemptProject, r)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseWrite := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWrite)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- f.store.BackgroundWrite(context.Background(), func(ctx context.Context, _ *sql.Tx) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owned startup fixture writer did not enter")
	}
	type startup struct {
		done <-chan error
		err  error
	}
	started := make(chan startup, 1)
	driver := &controlledDriver{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done, err := f.e.Start(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
		started <- startup{done, err}
	}()
	select {
	case result := <-started:
		t.Fatal("startup returned before its recovery transaction could run", result.err)
	case <-time.After(80 * time.Millisecond):
	}
	releaseWrite()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owned writer did not release")
	}
	var result startup
	select {
	case result = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("startup did not complete after writer release")
	}
	if result.err != nil || result.done == nil || supervisorOperationState(t, f, r.ID) != "cancelled" || driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
		t.Fatal("startup did not commit recovery before success", result.err)
	}
	requireSupervisorCompletion(t, f, cancel, result.done)
}

func TestSupervisorConcurrentStartOwnsOnePoolAndCopiesBindings(t *testing.T) {
	f := supervisorFixture(t)
	driver := &controlledDriver{}
	drivers := map[string]Driver{"test-capability": driver, "other-account": driver}
	if done, err := f.e.Start(nil, drivers); done != nil || err == nil || f.e.running.Load() {
		t.Fatal("nil context admitted a supervisor")
	}
	interrupted, interrupt := context.WithCancel(context.Background())
	interrupt()
	if done, err := f.e.Start(interrupted, drivers); done != nil || err == nil || f.e.running.Load() {
		t.Fatal("cancelled startup retained ownership or returned workers")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type startup struct {
		done <-chan error
		err  error
	}
	results := make(chan startup, 8)
	start := make(chan struct{})
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() { <-start; done, err := f.e.Start(ctx, drivers); results <- startup{done, err} })
	}
	close(start)
	callers.Wait()
	var winner <-chan error
	for range 8 {
		got := <-results
		if got.err == nil {
			if winner != nil || got.done == nil {
				t.Fatal("multiple owners or missing completion channel")
			}
			winner = got.done
		} else {
			attemptCode(t, got.err, "review_conflict")
			if got.done != nil {
				t.Fatal("rejected start returned worker channel")
			}
		}
	}
	if winner == nil {
		t.Fatal("no concurrent startup succeeded")
	}
	// Mutating the caller's map after Start returns must not affect the pool.
	delete(drivers, "test-capability")
	drivers["other-account"] = nil
	r := f.enqueue(t, attemptProject, "copied-startup-drivers", "cli", 2, 1)
	waitOperationState(t, f, r.ID, "completed")
	if driver.prepares.Load() != 1 || driver.calls.Load() != 1 {
		t.Fatal("owned driver binding changed or redispatched")
	}
	requireSupervisorCompletion(t, f, cancel, winner)
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

func TestSupervisorPreparationReasonsAreSafeAndInspectable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"pending", core.Fail("review_evidence_pending", "private diagnostic", 409), "review_evidence_pending"},
		{"covered", &core.Error{Code: "review_held", Details: map[string]string{"reason": "target_already_covered"}}, "target_already_covered"},
		{"unavailable", &core.Error{Code: "review_held", Details: map[string]string{"reason": "source_unavailable"}}, "source_unavailable"},
		{"unsafe details", &core.Error{Code: "review_held", Details: map[string]string{"reason": "private_path_or_token"}}, "preparation_failed"},
		{"unrelated code", &core.Error{Code: "other", Details: map[string]string{"reason": "target_already_covered"}}, "preparation_failed"},
		{"raw error", errors.New("private provider diagnostic"), "preparation_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := supervisorFixture(t)
			driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
				return Preparation{}, tc.failure
			}}
			stop := runSupervisor(t, f, driver)
			receipt, _ := supervisorRequest(t, f, "inspect-held-reason", "cli", 2)
			waitOperationState(t, f, receipt.ID, "held")
			stop()
			response := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/operations/"+receipt.ID, f.reader, "", "")
			var operation Operation
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &operation) != nil || operation.Reason != tc.want {
				t.Fatalf("held classification: %d %s", response.Code, response.Body.String())
			}
			if driver.calls.Load() != 0 || strings.Contains(response.Body.String(), "private") {
				t.Fatal("unsafe hold diagnostic or provider dispatch")
			}
		})
	}
}
