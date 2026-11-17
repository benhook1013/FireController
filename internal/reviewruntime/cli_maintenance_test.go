package reviewruntime

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLIMaintenanceUsesExistingWorkerWithoutProviderEffects(t *testing.T) {
	f := newCLIDriverFixture(t)
	if f.driver.workspaceMaintenance != nil {
		t.Fatal("ordinary driver enabled maintenance")
	}
	if _, err := NewCLIDriverWithMaintenance(f.store, f.engine, f.capability, f.manager, f.captures, f.reader, f.runner, nil); err == nil {
		t.Fatal("nil opt-in accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	var calls atomic.Int64
	driver, err := NewCLIDriverWithMaintenance(f.store, f.engine, f.capability, f.manager, f.captures, f.reader, f.runner, func(c context.Context) error {
		deadline, ok := c.Deadline()
		if !ok || time.Until(deadline) > 20*time.Second {
			t.Error("missing original bounded context")
		}
		calls.Add(1)
		close(entered)
		<-c.Done()
		return c.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := driver.StartWorkspaceCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("hook not reached")
	}
	if _, err := f.driver.StartWorkspaceCleanup(ctx); err == nil {
		t.Fatal("duplicate worker admitted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance ignored shutdown")
	}
	for range done {
	}
	if calls.Load() != 1 || f.fetches.Load() != 0 {
		t.Fatal("callback retried or provider fetch", calls.Load(), f.fetches.Load())
	}
	f.reader.mu.Lock()
	providerCalls := f.reader.repositoryCalls + len(f.reader.calls)
	f.reader.mu.Unlock()
	if providerCalls != 0 {
		t.Fatal("provider read during maintenance", providerCalls)
	}
	body, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal("cannot verify provider process absence", err)
	}
	if len(body) != 0 {
		t.Fatal("provider process executed")
	}
}
