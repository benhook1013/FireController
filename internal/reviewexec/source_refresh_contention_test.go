package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func refreshContentionLock(t *testing.T, f *attemptFixture) *sql.Tx {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), `UPDATE projects SET name=name WHERE id=?`, attemptProject); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}
func refreshContentionLoop(t *testing.T, f *attemptFixture, reader TargetReader) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- f.e.sourceRefreshLoop(ctx, map[string]TargetReader{"test-capability": reader})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("contention loop leaked")
		}
	})
	return cancel, done
}
func refreshContentionAlive(t *testing.T, done <-chan error, hold time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatal("transient contention killed source worker", err)
	case <-time.After(hold):
	}
}
func refreshContentionReader(t *testing.T) (*refreshTestReader, <-chan struct{}, chan<- struct{}) {
	t.Helper()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	r := &refreshTestReader{pull: func(ctx context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return reviewprovider.Pull{}, ctx.Err()
		}
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	return r, entered, release
}
func refreshContentionEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider not reached")
	}
}

func TestSourceRefreshContentionBeforeClaimKeepsQueuedWork(t *testing.T) {
	for _, kind := range []string{"writer", "reader_pool"} {
		t.Run(kind, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			receipt := refreshEnqueue(t, f, "contention-before-claim", refreshRequest())
			r := &refreshTestReader{}
			var release func()
			hold := 250 * time.Millisecond
			if kind == "writer" {
				tx := refreshContentionLock(t, f)
				release = func() {
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				f.store.DB().SetMaxOpenConns(1)
				conn, err := f.store.DB().Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				release = func() { conn.Close() }
				hold = 2200 * time.Millisecond
			}
			cancel, done := refreshContentionLoop(t, f, r)
			refreshContentionAlive(t, done, hold)
			if r.calls.Load() != 0 {
				t.Fatal("provider read before local claim")
			}
			release()
			result := refreshWait(t, f, receipt.ID, "completed")
			if result.Revision != 3 || r.calls.Load() != 2 {
				t.Fatal("queued work did not recover once", result, r.calls.Load())
			}
			refreshStop(t, cancel, done)
		})
	}
}

func TestSourceRefreshContentionAfterReadRetainsResultAndProgresses(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "contention-after-read", refreshRequest())
	r, entered, release := refreshContentionReader(t)
	cancel, done := refreshStart(t, f, r)
	refreshContentionEntered(t, entered)
	tx := refreshContentionLock(t, f)
	close(release)
	refreshContentionAlive(t, done, 250*time.Millisecond)
	if got := refreshRead(t, f, receipt.ID); got.State != "running" || got.Observation != nil || r.calls.Load() != 2 {
		t.Fatal("pending completion reread/changed source", got, r.calls.Load())
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	result := refreshWait(t, f, receipt.ID, "completed")
	if result.Revision != 3 || result.Observation == nil || r.calls.Load() != 2 {
		t.Fatal("completion was stranded or repeated provider read", result, r.calls.Load())
	}
	next := refreshRequest()
	next.PullNumber = 4
	second := refreshEnqueue(t, f, "contention-next", next)
	refreshWait(t, f, second.ID, "completed")
	if r.calls.Load() != 4 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.source_refresh_updated'`) != 4 {
		t.Fatal("queue stalled or duplicated effects", r.calls.Load())
	}
	refreshStop(t, cancel, done)
}

func TestSourceRefreshContentionCompletionRechecksCurrentBinding(t *testing.T) {
	for _, change := range []string{"revoked", "configuration", "pause-only"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			receipt := refreshEnqueue(t, f, "binding-during-contention", refreshRequest())
			r, entered, release := refreshContentionReader(t)
			cancel, done := refreshStart(t, f, r)
			refreshContentionEntered(t, entered)
			tx := refreshContentionLock(t, f)
			close(release)
			refreshContentionAlive(t, done, 250*time.Millisecond)
			if change == "revoked" {
				if _, err := tx.ExecContext(t.Context(), `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
					t.Fatal(err)
				}
			} else {
				previous := int64(1)
				c := Configuration{ExpectedRevision: &previous, CapabilityID: "other-account", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}
				if change == "pause-only" {
					c.CapabilityID = "test-capability"
					c.Paused = true
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,2,?)`, attemptProject, encoded(c)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			state, reason := "held", "source_configuration_changed"
			if change == "revoked" {
				reason = "authorization_changed"
			}
			if change == "pause-only" {
				state, reason = "completed", ""
			}
			got := refreshWait(t, f, receipt.ID, state)
			if got.Reason != reason || got.Revision != 3 || (got.Observation != nil) != (state == "completed") || r.calls.Load() != 2 {
				t.Fatal("completion reused stale authority or reread provider", got, r.calls.Load())
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.source_refresh_updated'`) != 2 {
				t.Fatal("completion appended duplicate events")
			}
			refreshStop(t, cancel, done)
		})
	}
}

func TestSourceRefreshContentionCancellationRetainsRunningForRestart(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "cancel-retained-result", refreshRequest())
	r, entered, release := refreshContentionReader(t)
	cancel, done := refreshStart(t, f, r)
	refreshContentionEntered(t, entered)
	tx := refreshContentionLock(t, f)
	close(release)
	refreshContentionAlive(t, done, 250*time.Millisecond)
	refreshStop(t, cancel, done)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got := refreshRead(t, f, receipt.ID)
	if got.State != "running" || got.Revision != 2 || got.Observation != nil || r.calls.Load() != 2 {
		t.Fatal("cancellation invented terminal result", got, r.calls.Load())
	}
	resumed := &refreshTestReader{}
	cancel, done = refreshStart(t, f, resumed)
	got = refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.Revision != 5 || resumed.calls.Load() != 2 {
		t.Fatal("normal startup recovery lost operation", got, resumed.calls.Load())
	}
	raw, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), "cancel-retained-result", refreshRequest())
	if err != nil || string(raw) != encoded(receipt) {
		t.Fatal("restart changed original receipt", string(raw), err)
	}
}

func TestSourceRefreshContentionProviderFailureIsRetainedAsHold(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "failed-provider-contention", refreshRequest())
	entered, release := make(chan struct{}), make(chan struct{})
	r := &refreshTestReader{repository: func(ctx context.Context, _ int64) (reviewprovider.Repository, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return reviewprovider.Repository{}, ctx.Err()
		}
		return reviewprovider.Repository{}, errors.New("private provider diagnostic")
	}}
	cancel, done := refreshStart(t, f, r)
	refreshContentionEntered(t, entered)
	tx := refreshContentionLock(t, f)
	close(release)
	refreshContentionAlive(t, done, 250*time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got := refreshWait(t, f, receipt.ID, "held")
	refreshStop(t, cancel, done)
	if got.Reason != "source_unavailable" || got.Observation != nil || got.Revision != 3 || r.calls.Load() != 1 {
		t.Fatal("provider failure was reread or misrecorded", got, r.calls.Load())
	}
}

func TestSourceRefreshContentionDoesNotHideCompletionStorageFailure(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "fatal-completion", refreshRequest())
	r, entered, release := refreshContentionReader(t)
	cancel, done := refreshStart(t, f, r)
	defer cancel()
	refreshContentionEntered(t, entered)
	admissionSQL(t, f, `CREATE TRIGGER reject_source_completion BEFORE UPDATE ON review_source_refreshes WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'private storage diagnostic'); END`)
	close(release)
	select {
	case err := <-done:
		attemptCode(t, err, "source_refresh_persistence_failed")
		if strings.Contains(err.Error(), "private") {
			t.Fatal("private storage error leaked")
		}
	case <-time.After(time.Second):
		t.Fatal("genuine storage failure was retried/hidden")
	}
	got := refreshRead(t, f, receipt.ID)
	if got.State != "running" || got.Revision != 2 || got.Observation != nil || r.calls.Load() != 2 || f.e.sourceRunning.Load() {
		t.Fatal("fatal failure changed state or repeated provider work", got)
	}
}

func TestSourceRefreshContentionBackoffIsBoundedAndCancellationPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 350*time.Millisecond)
	defer cancel()
	calls := 0
	err := retrySourceRefreshLocal(ctx, func() error { calls++; return core.Fail("overloaded", "Storage is busy.", 503) })
	if !errors.Is(err, context.DeadlineExceeded) || calls < 1 || calls > 4 {
		t.Fatal("contention busy-loop or lost cancellation", calls, err)
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	calls = 0
	if err = retrySourceRefreshLocal(stopped, func() error { calls++; return nil }); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("pre-cancel invoked local transaction", calls, err)
	}
	failure := core.Fail("storage_unavailable", "Safe storage failure.", 500)
	calls = 0
	if err = retrySourceRefreshLocal(t.Context(), func() error { calls++; return failure }); err != failure || calls != 1 {
		t.Fatal("non-overload failure retried", calls, err)
	}
}

func TestSourceRefreshContentionAfterReadSurvivesRealWriterAdmissionDeadline(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "admission-after-read", refreshRequest())
	r, entered, release := refreshContentionReader(t)
	cancel, done := refreshStart(t, f, r)
	refreshContentionEntered(t, entered)
	held, unlock, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		holderDone <- f.store.BackgroundWrite(t.Context(), func(context.Context, *sql.Tx) error { close(held); <-unlock; return nil })
		close(holderDone)
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("fixture writer was not admitted")
	}
	// Ensure fixture cleanup releases its owned writer before Store.Close on failure.
	released := false
	releaseWriter := func() {
		if !released {
			close(unlock)
			released = true
		}
		select {
		case err := <-holderDone:
			if err != nil {
				attemptCode(t, err, "overloaded")
			}
		case <-time.After(time.Second):
			t.Fatal("fixture writer leaked")
		}
	}
	t.Cleanup(releaseWriter)
	close(release)
	// BackgroundWrite's real two-second admission deadline expires at least once.
	refreshContentionAlive(t, done, 2200*time.Millisecond)
	if r.calls.Load() != 2 {
		t.Fatal("admission timeout repeated provider reads", r.calls.Load())
	}
	releaseWriter()
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.Revision != 3 || r.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.source_refresh_updated'`) != 2 {
		t.Fatal("admission retry lost/duplicated completion", got, r.calls.Load())
	}
}
