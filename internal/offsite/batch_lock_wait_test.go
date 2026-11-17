package offsite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBatchLockWaitPreservesScheduledAttempt(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	if err := privateDir(b.StateDir, true); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(filepath.Join(b.StateDir, "batch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err = lock(held); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := pushBatchWithLockWait(t.Context(), b, cs, ts, time.Second); done <- e }()
	select {
	case e := <-done:
		t.Fatal("scheduled attempt did not wait", e)
	case <-time.After(150 * time.Millisecond):
	}
	for _, c := range cs {
		if _, e := os.Stat(c.StateDir); !os.IsNotExist(e) {
			t.Fatal("provider mutated before custody", e)
		}
	}
	if err = held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resume")
	}
	for _, c := range cs {
		s, e := readState(c)
		if e != nil || s.LastSuccess == nil {
			t.Fatal("scheduled backup lost", e)
		}
	}
}
func TestBatchLockWaitRefusesWithoutWorkspaceChanges(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			_, b, cs, ts := batchFixture(t)
			if err := privateDir(b.StateDir, true); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(b.StateDir, "batch.lock")
			held, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			if e != nil {
				t.Fatal(e)
			}
			defer held.Close()
			if e = lock(held); e != nil {
				t.Fatal(e)
			}
			marker := filepath.Join(b.StateDir, "existing-proof")
			if e = os.WriteFile(marker, []byte("retain"), 0600); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, e := pushBatchWithLockWait(ctx, b, cs, ts, 350*time.Millisecond); done <- e }()
			time.Sleep(120 * time.Millisecond)
			want := "offsite_admission_busy"
			if mode == "cancel" {
				cancel()
				want = "offsite_cancelled"
			}
			if mode == "replacement" {
				if e = os.Rename(path, path+".original"); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(path, nil, 0600); e != nil {
					t.Fatal(e)
				}
				held.Close()
				want = "offsite_storage_unavailable"
			}
			select {
			case e = <-done:
				wantCode(t, e, want)
			case <-time.After(2 * time.Second):
				t.Fatal("unbounded lock wait")
			}
			raw, e := os.ReadFile(marker)
			if e != nil || string(raw) != "retain" {
				t.Fatal("changed proof")
			}
			for _, c := range cs {
				if _, e := os.Stat(c.StateDir); !os.IsNotExist(e) {
					t.Fatal("provider mutated", e)
				}
			}
		})
	}
}
