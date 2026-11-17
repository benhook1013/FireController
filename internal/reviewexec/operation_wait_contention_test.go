package reviewexec

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestOperationWaitReadAdmissionContentionReauthorizesOriginalResult(t *testing.T) {
	f := sourceHTTPFixture(t)
	r := f.enqueue(t, attemptProject, "wait-read-admission", "cli", 2, 1)
	a := f.start(t, attemptProject, r)
	f.store.DB().SetMaxOpenConns(1)
	reader := operationReadScope(t, f)
	writer := f.scope(t, attemptProject, f.writer, false)
	allowed, _ := startOperationWait(t, f, writer, r.ID, 10)
	revoked, _ := startOperationWait(t, f, reader, r.ID, 10)
	operationWaitRegistered(t, f.e, 2)
	// Consuming a hint proves that each initial authorized read completed.
	f.e.notifyOperation(r.ID)
	readyBy := time.Now().Add(2 * time.Second)
	for {
		f.e.operationMu.Lock()
		ready := true
		for waiter := range f.e.operationWaiters[r.ID] {
			ready = ready && len(waiter.changed) == 0
		}
		f.e.operationMu.Unlock()
		if ready {
			break
		}
		if time.Now().After(readyBy) {
			t.Fatal("initial operation read did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "cancelled", "controlled_cancel", ""); err != nil {
		t.Fatal(err)
	}
	// The committed notification forces reads while the real pool remains full
	// beyond Store.Read's two-second admission deadline.
	time.Sleep(2250 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	result := receiveOperationWait(t, allowed)
	if result.err != nil || !result.terminal || result.value.ID != r.ID || result.value.State != "cancelled" {
		t.Fatal("temporary read admission ended the original long wait", result, result.err)
	}
	result = receiveOperationWait(t, revoked)
	attemptCode(t, result.err, "unauthorized")
	if result.value.ID != "" || result.terminal {
		t.Fatal("read retry exposed a result after authorization was revoked", result)
	}
	operationWaitRegistered(t, f.e, 0)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, a.ID) != 2 {
		t.Fatal("result collection changed original execution history")
	}
}

func TestOperationWaitImmediateReadAdmissionIsNotRetried(t *testing.T) {
	f := sourceHTTPFixture(t)
	r := f.enqueue(t, attemptProject, "immediate-read-admission", "cli", 2, 1)
	scope := operationReadScope(t, f)
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	_, terminal, err := f.e.WaitOperation(t.Context(), scope, r.ID, 0)
	attemptCode(t, err, "overloaded")
	if terminal || time.Since(started) > 3*time.Second {
		t.Fatal("immediate result read retried pool admission", terminal, time.Since(started))
	}
}
