package offsite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backupphase"
)

// The caller's single deadline is not reset between batch and phase admission.
// This is prerequisite proof, not a configured production cycle budget.
func TestBatchCycleDeadlineSpansBothAdmissionLocks(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	if err := privateDir(b.StateDir, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.StateDir, "batch.lock")
	held, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err = lock(held); err != nil {
		t.Fatal(err)
	}
	b.PhaseLease = filepath.Join(t.TempDir(), "phase.lock")
	if err = os.WriteFile(b.PhaseLease, nil, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := backupphase.Acquire(t.Context(), b.PhaseLease)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := pushBatchWithLockWait(ctx, b, cs, ts, time.Second); done <- e }()
	time.Sleep(150 * time.Millisecond)
	if err = held.Close(); err != nil {
		t.Fatal(err)
	}
	// Observe a different descriptor losing flock only after our original holder
	// is closed. This proves the batch goroutine owns custody while phase is held.
	observedBatch := false
	for ctx.Err() == nil {
		probe, e := os.OpenFile(path, os.O_RDWR, 0600)
		if e != nil {
			t.Fatal(e)
		}
		e = lock(probe)
		probe.Close()
		if e != nil {
			observedBatch = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !observedBatch {
		t.Fatal("never observed batch custody before phase deadline")
	}
	select {
	case err = <-done:
		wantCode(t, err, "offsite_cancelled")
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatal("cycle deadline not respected", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("phase admission reset cycle deadline")
	}
	for _, c := range cs {
		if _, err = os.Stat(c.StateDir); !os.IsNotExist(err) {
			t.Fatal("provider work before complete admission", err)
		}
	}
	if _, err = os.Stat(filepath.Join(b.StateDir, "work")); !os.IsNotExist(err) {
		t.Fatal("staging before complete admission", err)
	}
	check, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err = lock(check); err != nil {
		t.Fatal("batch custody leaked after deadline", err)
	}
}
