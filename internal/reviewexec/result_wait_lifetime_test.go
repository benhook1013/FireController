package reviewexec

import (
	"context"
	"testing"
	"time"
)

func TestCheckpointWaitUsesResultWaitLifetime(t *testing.T) {
	f, _, evidenceID := checkpointFixture(t, 0)
	receipt, _ := checkpointRequest(t, f, evidenceID, "lifecycle-checkpoint")
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	e, err := New(f.store, nil, WithResultWaitLifetime(lifetime))
	if err != nil {
		t.Fatal(err)
	}
	before := checkpointSQLState(t, f)
	if _, _, err := e.WaitCheckpointPublication(t.Context(), scope, receipt.ID, 901); err == nil {
		t.Fatal("checkpoint accepted wait beyond finite bound")
	}
	callerCtx, cancelCaller := context.WithCancel(t.Context())
	defer cancelCaller()
	done := make(chan error, 1)
	go func() {
		_, _, waitErr := e.WaitCheckpointPublication(callerCtx, scope, receipt.ID, 900)
		done <- waitErr
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.checkpointMu.Lock()
		count := e.checkpointWaiterCount
		e.checkpointMu.Unlock()
		if count == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	e.checkpointMu.Lock()
	registered := e.checkpointWaiterCount == 1
	e.checkpointMu.Unlock()
	if !registered {
		t.Fatal("checkpoint wait was not registered")
	}
	cancelLifetime()
	select {
	case waitErr := <-done:
		attemptCode(t, waitErr, "overloaded")
	case <-time.After(time.Second):
		cancelCaller()
		t.Fatal("lifecycle did not release checkpoint result wait")
	}
	e.checkpointMu.Lock()
	remaining := e.checkpointWaiterCount
	e.checkpointMu.Unlock()
	if remaining != 0 || checkpointSQLState(t, f) != before {
		t.Fatal("lifecycle wait cancellation leaked capacity or changed publication state", remaining)
	}
}

func TestSourceRefreshWaitUsesResultWaitLifetime(t *testing.T) {
	f := sourceHTTPFixture(t)
	receipt := refreshEnqueue(t, f, "lifecycle-source-refresh", refreshRequest())
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	e, err := New(f.store, nil, WithResultWaitLifetime(lifetime))
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	beforeReceipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.source_refresh'`)
	if _, err := e.WaitSourceRefresh(t.Context(), scope, receipt.ID, 901*time.Second); err == nil {
		t.Fatal("source refresh accepted wait beyond finite bound")
	}
	callerCtx, cancelCaller := context.WithCancel(t.Context())
	defer cancelCaller()
	done := make(chan error, 1)
	go func() {
		_, waitErr := e.WaitSourceRefresh(callerCtx, scope, receipt.ID, 900*time.Second)
		done <- waitErr
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.sourceMu.Lock()
		count := e.sourceWaiterCount
		e.sourceMu.Unlock()
		if count == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	e.sourceMu.Lock()
	registered := e.sourceWaiterCount == 1
	e.sourceMu.Unlock()
	if !registered {
		t.Fatal("source refresh wait was not registered")
	}
	cancelLifetime()
	select {
	case waitErr := <-done:
		attemptCode(t, waitErr, "overloaded")
	case <-time.After(time.Second):
		cancelCaller()
		t.Fatal("lifecycle did not release source refresh result wait")
	}
	e.sourceMu.Lock()
	remaining := e.sourceWaiterCount
	e.sourceMu.Unlock()
	state := refreshRead(t, f, receipt.ID)
	if remaining != 0 || state.State != "queued" || state.Revision != receipt.Revision || attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.source_refresh'`) != beforeReceipts {
		t.Fatal("lifecycle wait cancellation leaked capacity or changed source refresh state", remaining, state)
	}
}
