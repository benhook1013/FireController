//go:build linux

package reviewprocess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheProducerActualRunnerResumesOnceAfterMaintenance(t *testing.T) {
	c := &CacheCustody{}
	c.mu.Lock()
	locked := true
	defer func() {
		if locked {
			c.mu.Unlock()
		}
	}()
	effect := filepath.Join(t.TempDir(), "effect")
	r := &Runner{cacheCustody: c, executable: "/bin/sh", env: []string{"EFFECT=" + effect}, now: time.Now}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan runResult, 1)
	go func() { done <- r.run(ctx, "/", []string{"-c", `printf x >> "$EFFECT"`}, 1024, 1024) }()
	deadline := time.Now().Add(time.Second)
	for c.producerWaiters.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.producerWaiters.Load() != 1 {
		t.Fatal("actual runner never waited")
	}
	if _, err := os.Stat(effect); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("producer ran during maintenance", err)
	}
	select {
	case got := <-done:
		t.Fatal("producer returned during maintenance", got)
	default:
	}
	c.mu.Unlock()
	locked = false
	got := <-done
	if got.err != nil || !got.capture.Started || got.capture.ExitCode != 0 {
		t.Fatal(got)
	}
	b, err := os.ReadFile(effect)
	if err != nil || string(b) != "x" {
		t.Fatal("producer replayed or missing", string(b), err)
	}
}
func TestCacheProducerHomeWaitRetainsDeadlineAndReleases(t *testing.T) {
	home := t.TempDir()
	lease, err := NewCacheHomeLease(home)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	hold, err := lease.acquire(true)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	f, err := lease.acquireProducer(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || f != nil {
		if f != nil {
			f.Close()
		}
		t.Fatal("busy admission did not honor original deadline", err)
	}
	hold.Close()
	f, err = lease.acquireProducer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
