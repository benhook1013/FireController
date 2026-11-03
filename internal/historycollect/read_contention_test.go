package historycollect

import (
	"context"
	"testing"
	"time"
)

func TestCollectionQuietWaitSurvivesReadAdmission(t *testing.T) {
	f := newCollectionStoreFixture(t)
	project, worker := f.project(t, "selected/repository")
	scope := f.scope(t, project, worker, f.admin)
	e := f.engine(t)
	receipt := f.enqueue(t, scope, "read-admission", Selection{[]int64{47}})
	before := collectionStoreRows(t, f)
	f.store.DB().SetMaxOpenConns(1)
	conn, err := f.store.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan collectionWaitResult, 1)
	go func() {
		value, err := e.Wait(ctx, scope, receipt.ID, 3*time.Second)
		done <- collectionWaitResult{value, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("wait ended before read-pool recovery: %v", got.err)
	case <-time.After(2200 * time.Millisecond):
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.operation.State != "queued" || got.operation.ID != receipt.ID {
			t.Fatal("wrong recovered result", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read recovery exceeded original wait")
	}
	collectionWaitCount(t, e, 0)
	if collectionStoreRows(t, f) != before || len(f.provider.calls) != 0 {
		t.Fatal("read retry mutated state or contacted provider")
	}
}
