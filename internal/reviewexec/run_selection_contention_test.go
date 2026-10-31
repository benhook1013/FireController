package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func selectionContentionLock(t *testing.T, f *attemptFixture) *sql.Tx {
	t.Helper()
	// The second selector may still be finishing its losing claim. Bound the
	// fixture's acquisition separately; contention after acquisition is the test.
	deadline := time.Now().Add(time.Second)
	for {
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		if err == nil {
			_, err = tx.ExecContext(t.Context(), `UPDATE projects SET name=name WHERE id=?`, attemptProject)
			if err == nil {
				t.Cleanup(func() { tx.Rollback() })
				return tx
			}
			tx.Rollback()
		}
		var coded interface{ Code() int }
		if !errors.As(err, &coded) || coded.Code()&255 != 5 && coded.Code()&255 != 6 || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
}
func selectionContentionStart(t *testing.T, f *attemptFixture, r TargetReader) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartReviewRuns(ctx, map[string]TargetReader{"test-capability": r})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("selection worker leaked")
		}
	})
	return cancel, done
}
func selectionContentionStop(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("selection worker failed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("selection worker did not join promptly")
	}
}
func selectionContentionAlive(t *testing.T, done <-chan error, hold time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatal("transient contention killed selection worker", err)
	case <-time.After(hold):
	}
}
func selectionContentionReader() (*refreshTestReader, <-chan struct{}, chan<- struct{}) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return reviewprovider.Pull{}, ctx.Err()
		}
		return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	return r, entered, release
}
func selectionContentionEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider phase not reached")
	}
}
func selectionContentionRead(t *testing.T, f *attemptFixture, id string) Run {
	t.Helper()
	got, err := f.e.ReadRun(t.Context(), selectionScope(t, f, attemptProject, f.admin, "read"), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func selectionContentionWait(t *testing.T, f *attemptFixture, id, state string) Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := selectionContentionRead(t, f, id)
		if got.State == state {
			return got
		}
		if got.State == "held" || got.State == "selected" || got.State == "empty" {
			t.Fatal("unexpected terminal selection", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("selection did not reach bounded state", state)
	return Run{}
}

func TestRunSelectionContentionAfterSourceRetainsFactsAndProgresses(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "contended-source", "cli", nil)
	r, entered, release := selectionContentionReader()
	cancel, done := selectionContentionStart(t, f, r)
	selectionContentionEntered(t, entered)
	tx := selectionContentionLock(t, f)
	close(release)
	selectionContentionAlive(t, done, 250*time.Millisecond)
	if got := selectionContentionRead(t, f, receipt.ID); got.State != "selecting" || got.Revision != 2 || r.calls.Load() != 3 {
		t.Fatal("pending completion changed source", got, r.calls.Load())
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got := selectionContentionWait(t, f, receipt.ID, "selected")
	if got.Revision != 3 || got.OperationID == "" || r.calls.Load() != 3 {
		t.Fatal("completion repeated provider reads or stranded selection", got, r.calls.Load())
	}
	second, _ := selectionEnqueue(t, f, "next-channel", "hosted", nil)
	selectionContentionWait(t, f, second.ID, "selected")
	selectionContentionStop(t, cancel, done)
	if r.calls.Load() != 6 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.selection_updated'`) != 4 {
		t.Fatal("later work stalled or child/event duplicated", r.calls.Load())
	}
}

func TestRunSelectionContentionBeforeClaimKeepsQueuedWork(t *testing.T) {
	for _, kind := range []string{"writer", "reader_pool"} {
		t.Run(kind, func(t *testing.T) {
			f := selectionFixture(t)
			receipt, _ := selectionEnqueue(t, f, "contended-claim", "cli", nil)
			r := &refreshTestReader{}
			var release func()
			hold := 250 * time.Millisecond
			if kind == "writer" {
				tx := selectionContentionLock(t, f)
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
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": r})
				done <- err
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("contended claim leaked")
				}
			})
			selectionContentionAlive(t, done, hold)
			if r.calls.Load() != 0 {
				t.Fatal("provider called before local claim", r.calls.Load())
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("queued claim stranded")
			}
			got := selectionContentionRead(t, f, receipt.ID)
			if got.State != "selected" || got.Revision != 3 || r.calls.Load() != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
				t.Fatal("claim retry duplicated or lost work", got, r.calls.Load())
			}
		})
	}
}

func TestRunSelectionContentionLocalReadRetainsSourceAndRechecksBinding(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "contended-read", "cli", nil)
	r, entered, release := selectionContentionReader()
	cancel, done := selectionContentionStart(t, f, r)
	selectionContentionEntered(t, entered)
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	close(release)
	selectionContentionAlive(t, done, 2200*time.Millisecond)
	if r.calls.Load() != 3 {
		t.Fatal("local read repeated source calls", r.calls.Load())
	}
	// Use the separately bounded writer while the read pool is exhausted. The
	// next local snapshot must reject this authority without another source read.
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	got := selectionContentionWait(t, f, receipt.ID, "held")
	selectionContentionStop(t, cancel, done)
	if got.Reason != "authorization_changed" || got.Revision != 3 || r.calls.Load() != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("local read reused stale binding or repeated source", got, r.calls.Load())
	}
}

func TestRunSelectionContentionLocalReadBeyondSourceBudgetRetainsFacts(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "prolonged-local-read", "cli", nil)
	r, entered, release := selectionContentionReader()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := f.e.runNext(ctx, map[string]TargetReader{"test-capability": r})
		done <- err
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("prolonged local-read selection leaked")
		}
	})
	selectionContentionEntered(t, entered)
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	close(release)
	// Exceed the real cumulative source budget while only the local read pool
	// is unavailable. Already acquired source facts must not become a timeout.
	select {
	case err := <-done:
		conn.Close()
		got := selectionContentionRead(t, f, receipt.ID)
		t.Fatal("local read contention produced an early terminal selection", got, err)
	case <-time.After(16 * time.Second):
	}
	var state, operation string
	var revision int64
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT state,revision,COALESCE(operation_id,'') FROM review_selections WHERE id=?`, receipt.ID).Scan(&state, &revision, &operation)
	}); err != nil {
		t.Fatal(err)
	}
	if state != "selecting" || revision != 2 || operation != "" || r.calls.Load() != 3 {
		t.Fatal("local contention invented a terminal result or repeated source", state, revision, operation, r.calls.Load())
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("selection failed after local contention cleared", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selection stranded after local contention cleared")
	}
	got := selectionContentionRead(t, f, receipt.ID)
	if got.State != "selected" || got.Revision != 3 || got.OperationID == "" || r.calls.Load() != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.selection_updated'`) != 2 {
		t.Fatal("prolonged local retry lost or duplicated selection authority", got, r.calls.Load())
	}
}

func TestRunSelectionContentionCompletionRechecksCurrentBinding(t *testing.T) {
	for _, change := range []string{"revoked", "configuration"} {
		t.Run(change, func(t *testing.T) {
			f := selectionFixture(t)
			receipt, _ := selectionEnqueue(t, f, "changed-binding", "cli", nil)
			r, entered, release := selectionContentionReader()
			cancel, done := selectionContentionStart(t, f, r)
			selectionContentionEntered(t, entered)
			tx := selectionContentionLock(t, f)
			close(release)
			selectionContentionAlive(t, done, 250*time.Millisecond)
			reason := "authorization_changed"
			if change == "revoked" {
				if _, err := tx.ExecContext(t.Context(), `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
					t.Fatal(err)
				}
			} else {
				previous := int64(1)
				c := Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: true}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO review_execution_config(project_id,revision,body_json) VALUES(?,2,?)`, attemptProject, encoded(c)); err != nil {
					t.Fatal(err)
				}
				reason = "configuration_changed"
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			got := selectionContentionWait(t, f, receipt.ID, "held")
			selectionContentionStop(t, cancel, done)
			if got.Reason != reason || got.Revision != 3 || r.calls.Load() != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.selection_updated'`) != 2 {
				t.Fatal("completion retry used stale binding or duplicated effects", got, r.calls.Load())
			}
		})
	}
}

func TestRunSelectionContentionCancellationRetainsSelectionForRestart(t *testing.T) {
	f := selectionFixture(t)
	receipt, original := selectionEnqueue(t, f, "cancel-selection-contention", "cli", nil)
	r, entered, release := selectionContentionReader()
	cancel, done := selectionContentionStart(t, f, r)
	selectionContentionEntered(t, entered)
	tx := selectionContentionLock(t, f)
	close(release)
	selectionContentionAlive(t, done, 250*time.Millisecond)
	selectionContentionStop(t, cancel, done)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got := selectionContentionRead(t, f, receipt.ID)
	if got.State != "selecting" || got.Revision != 2 || got.OperationID != "" || r.calls.Load() != 3 {
		t.Fatal("cancellation invented terminal work", got, r.calls.Load())
	}
	resumed := &refreshTestReader{}
	cancel, done = selectionContentionStart(t, f, resumed)
	got = selectionContentionWait(t, f, receipt.ID, "selected")
	selectionContentionStop(t, cancel, done)
	if got.Revision != 5 || resumed.calls.Load() != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
		t.Fatal("startup lost or duplicated interrupted selection", got, resumed.calls.Load())
	}
	raw, err := f.e.EnqueueRun(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-selection-contention", RunRequest{Channel: "cli"})
	if err != nil || string(raw) != string(original) {
		t.Fatal("restart changed original-key receipt", err)
	}
}

func TestRunSelectionContentionDoesNotHideCompletionStorageFailure(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "fatal-selection-completion", "cli", nil)
	r, entered, release := selectionContentionReader()
	_, done := selectionContentionStart(t, f, r)
	selectionContentionEntered(t, entered)
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER reject_selection_completion BEFORE UPDATE ON review_selections WHEN NEW.state='selected' BEGIN SELECT RAISE(ABORT,'private storage diagnostic'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		attemptCode(t, err, "review_selection_persistence_failed")
		if strings.Contains(err.Error(), "private") {
			t.Fatal("private diagnostic leaked")
		}
	case <-time.After(time.Second):
		t.Fatal("fatal storage failure swallowed or retried")
	}
	got := selectionContentionRead(t, f, receipt.ID)
	if got.State != "selecting" || got.Revision != 2 || r.calls.Load() != 3 || f.e.runRunning.Load() || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("fatal completion leaked a child or repeated provider work", got, r.calls.Load())
	}
}

func TestRunSelectionContentionAfterSourceSurvivesWriterAdmissionDeadline(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "contended-completion-admission", "cli", nil)
	r, entered, release := selectionContentionReader()
	cancel, done := selectionContentionStart(t, f, r)
	selectionContentionEntered(t, entered)
	held, unlock, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		holderDone <- f.store.BackgroundWrite(t.Context(), func(context.Context, *sql.Tx) error { close(held); <-unlock; return nil })
		close(holderDone)
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("fixture writer not admitted")
	}
	var once sync.Once
	releaseWriter := func() {
		once.Do(func() { close(unlock) })
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
	selectionContentionAlive(t, done, 2200*time.Millisecond)
	if r.calls.Load() != 3 {
		t.Fatal("writer admission deadline repeated source", r.calls.Load())
	}
	releaseWriter()
	got := selectionContentionWait(t, f, receipt.ID, "selected")
	selectionContentionStop(t, cancel, done)
	if got.Revision != 3 || r.calls.Load() != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.selection_updated'`) != 2 {
		t.Fatal("bounded admission retry lost/duplicated completion", got, r.calls.Load())
	}
}

func TestRunSelectionContentionBackoffCancellationAndFatalBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 350*time.Millisecond)
	defer cancel()
	calls := 0
	err := retryRunSelectionLocal(ctx, func() error { calls++; return core.Fail("overloaded", "Storage is busy.", 503) })
	if !errors.Is(err, context.DeadlineExceeded) || calls < 1 || calls > 4 {
		t.Fatal("backoff spun or lost cancellation", calls, err)
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	calls = 0
	if err = retryRunSelectionLocal(stopped, func() error { calls++; return nil }); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("pre-cancel invoked local work", calls, err)
	}
	failure := core.Fail("storage_unavailable", "Safe storage failure.", 500)
	calls = 0
	if err = retryRunSelectionLocal(t.Context(), func() error { calls++; return failure }); err != failure || calls != 1 {
		t.Fatal("non-overload failure retried", calls, err)
	}
}
