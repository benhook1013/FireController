package offsite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backupphase"
)

// Models the proposed coordinator ordering with real flock descriptors, not a
// modified live coordinator. Managed backup holds phase, coordinator owns batch
// before waiting for phase, and the scheduled batch waits instead of losing tick.
func TestBatchLockWaitOrderedCoordinatorPreservesScheduledBackup(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	b.PhaseLease = filepath.Join(t.TempDir(), "phase.lock")
	if e := os.WriteFile(b.PhaseLease, nil, 0600); e != nil {
		t.Fatal(e)
	}
	releaseManaged, e := backupphase.Acquire(t.Context(), b.PhaseLease)
	if e != nil {
		t.Fatal(e)
	}
	defer releaseManaged()
	if e = privateDir(b.StateDir, true); e != nil {
		t.Fatal(e)
	}
	batch, e := os.OpenFile(filepath.Join(b.StateDir, "batch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer batch.Close()
	if e = lock(batch); e != nil {
		t.Fatal(e)
	}
	coordinator := make(chan func(), 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go func() {
		release, e := backupphase.Acquire(ctx, b.PhaseLease)
		if e != nil {
			coordinator <- nil
			return
		}
		coordinator <- release
	}()
	result := make(chan error, 1)
	go func() { _, e := pushBatchWithLockWait(ctx, b, cs, ts, 2*time.Second); result <- e }()
	select {
	case <-coordinator:
		t.Fatal("coordinator bypassed primary phase")
	case <-result:
		t.Fatal("scheduled backup lost while waiting")
	case <-time.After(120 * time.Millisecond):
	}
	releaseManaged()
	var releaseCoordinator func()
	select {
	case releaseCoordinator = <-coordinator:
		if releaseCoordinator == nil {
			t.Fatal("coordinator admission failed")
		}
	case <-time.After(time.Second):
		t.Fatal("lock-order cycle")
	}
	select {
	case <-result:
		t.Fatal("batch bypassed coordinator")
	default:
	}
	releaseCoordinator()
	batch.Close()
	select {
	case e = <-result:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("scheduled backup did not follow release")
	}
	for _, c := range cs {
		s, e := readState(c)
		if e != nil || s.LastSuccess == nil {
			t.Fatal("provider missed scheduled attempt", e)
		}
	}
}
