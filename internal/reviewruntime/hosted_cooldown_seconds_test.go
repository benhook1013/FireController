package reviewruntime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestHostedCooldownSecondsNativeWaitDueFreshQueryAndDispatch(t *testing.T) {
	f, now := newHostedCooldownRetryFixture(t)
	f.provider.reply = strings.Replace(runtimeAllowanceAvailable, "Reviews are available now.", "More reviews will be available in 12 seconds.", 1)
	op, first, config := f.startHosted(t)
	waiting := scheduleRuntimeCooldown(t, f, op, first, config)
	records, err := f.engine.ReadHostedObservations(t.Context(), f.project, first.ID)
	if err != nil || len(records) != 1 {
		t.Fatal("original seconds capture", records, err)
	}
	original, _ := json.Marshal(records)
	due, err := time.Parse(time.RFC3339Nano, waiting.Wait.RetryNotBefore)
	if err != nil {
		t.Fatal(err)
	}
	response := f.provider.comments[len(f.provider.comments)-1]
	if due.Sub(response.CreatedAt) != 13*time.Second {
		t.Fatal("seconds rounding changed", due.Sub(response.CreatedAt))
	}
	*now = due.Add(-time.Nanosecond)
	if early, err := f.engine.StartAttempt(t.Context(), f.project, op.ID); err != nil || early.ID != "" || f.provider.queries != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("early seconds deadline dispatched/reserved", early, err)
	}
	op, next, config := dueRuntimeCooldown(t, f, now, waiting)
	if next.ID == first.ID || next.ID == "" || next.OperationID != first.OperationID {
		t.Fatal("seconds retry identity", next)
	}
	f.provider.reply = runtimeAllowanceAvailable
	prepared, err := f.hosted.Prepare(t.Context(), op, config, next)
	if err != nil || f.provider.queries != 2 || f.provider.reviews != 0 {
		t.Fatal("seconds retry fresh query", err)
	}
	baseline, err := f.engine.RetryBaseline(t.Context(), f.project, op.ID)
	if err != nil || baseline == nil || !sameHostedRetryComparison(*baseline, prepared.Anchor) {
		t.Fatal("seconds retry original comparison", baseline, err)
	}
	next, err = f.engine.Admit(t.Context(), f.project, next.ID, next.Revision, prepared.Anchor, prepared.Intent)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.hosted.Dispatch(t.Context(), op, next, f.callback(t, &next))
	if err != nil || result.State != "completed" || f.provider.reviews != 1 {
		t.Fatal("seconds retry native dispatch", result, err)
	}
	after, err := f.engine.ReadHostedObservations(t.Context(), f.project, first.ID)
	afterBytes, _ := json.Marshal(after)
	if err != nil || string(original) != string(afterBytes) || hostedDriverJournal(t, f, first.ID) != "held" {
		t.Fatal("seconds retry changed original capture/history", err)
	}
	if _, err := f.hosted.Dispatch(t.Context(), op, next, f.callback(t, &next)); err != nil || f.provider.reviews != 1 {
		t.Fatal("seconds retry dispatched twice", err)
	}
}

// Advance only the scheduler clock after native preparation retains its proven
// response, modeling expiry before the supervisor can schedule the wait.
type expireSecondsBeforeSchedule struct {
	*HostedDriver
	clock *atomic.Int64
}

func (d *expireSecondsBeforeSchedule) Prepare(ctx context.Context, op reviewexec.Operation, c reviewexec.Configuration, a reviewexec.Attempt) (reviewexec.Preparation, error) {
	prepared, err := d.HostedDriver.Prepare(ctx, op, c, a)
	if err != nil {
		var stamp string
		if readErr := d.store.DB().QueryRowContext(ctx, `SELECT json_extract(body_json,'$.input.observation.RetryNotBefore') FROM review_hosted_preflight WHERE attempt_id=?`, a.ID).Scan(&stamp); readErr != nil {
			return reviewexec.Preparation{}, readErr
		}
		at, parseErr := time.Parse(time.RFC3339Nano, stamp)
		if parseErr != nil {
			return reviewexec.Preparation{}, parseErr
		}
		d.clock.Store(at.UnixNano())
	}
	return prepared, err
}
func TestHostedCooldownSecondsExpiredDuringPreparationStaysTerminal(t *testing.T) {
	f := newHostedDriverFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection(), reviewexec.WithExecutionClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() }))
	if err != nil {
		t.Fatal(err)
	}
	f.engine, f.hosted.engine = engine, engine
	f.hosted.provider = &cooldownRetryProvider{f.provider}
	f.provider.reply = strings.Replace(runtimeAllowanceAvailable, "Reviews are available now.", "More reviews will be available in 12 seconds.", 1)
	raw, err := engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "elapsed-seconds", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: &expireSecondsBeforeSchedule{HostedDriver: f.hosted, clock: &clock}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-done })
	result, terminal, err := engine.WaitOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID, 5)
	cancel()
	if workerErr := <-done; workerErr != nil {
		t.Fatal(workerErr)
	}
	if err != nil || !terminal || result.State != "held" || result.Reason != "hosted_allowance_cooldown" || result.Wait != nil || result.Allowance == nil || result.Allowance.Kind != "hosted_cooldown" {
		t.Fatal("elapsed seconds did not stay explicit terminal", result, terminal, err)
	}
	if f.provider.queries != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_operation_waits`) != 0 {
		t.Fatal("expired seconds busy-retried, retained capacity or claimed completion")
	}
	original := result
	if err := engine.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID)
	if err != nil || !reflect.DeepEqual(original, replay) {
		t.Fatal("historical elapsed hold reopened", replay, err)
	}
}
