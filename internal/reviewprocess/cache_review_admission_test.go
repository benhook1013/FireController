package reviewprocess

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheReviewRevalidatesConsumedPermitAfterWait(t *testing.T) {
	for _, mode := range []string{"expired", "valid", "workspace_changed", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newProcessFixture(t)
			marker := filepath.Join(f.dir, "review-started")
			r := f.runner(t, "echo started >> "+marker)
			var elapsed atomic.Int64
			issued := time.Unix(1700000000, 0)
			r.now = func() time.Time { return issued.Add(time.Duration(elapsed.Load())) }
			r.permit = &usagePermit{workspace: keyFor(f.workspace), issued: issued}
			c := &CacheCustody{}
			r.cacheCustody = c
			c.mu.Lock()
			locked := true
			defer func() {
				if locked {
					c.mu.Unlock()
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var starts atomic.Int64
			type outcome struct {
				capture Capture
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				capture, err := r.ReviewWithStart(ctx, f.workspace, func(time.Time) { starts.Add(1) })
				done <- outcome{capture, err}
			}()
			deadline := time.Now().Add(3 * time.Second)
			for c.producerWaiters.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if c.producerWaiters.Load() != 1 {
				t.Fatal("review did not reach custody wait")
			}
			switch mode {
			case "expired":
				elapsed.Store(int64(permitLifetime + time.Second))
			case "valid":
				elapsed.Store(int64(time.Second))
			case "workspace_changed":
				if err := os.WriteFile(filepath.Join(f.workspace.Directory, "source.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			c.mu.Unlock()
			locked = false
			var got outcome
			select {
			case got = <-done:
			case <-time.After(6 * time.Second):
				t.Fatal("review did not return")
			}
			if mode == "valid" {
				b, err := os.ReadFile(marker)
				if got.err != nil || !got.capture.Started || starts.Load() != 1 || err != nil || string(b) != "started\n" {
					t.Fatal(got, starts.Load(), string(b), err)
				}
			} else {
				if got.err == nil || got.capture.Started || starts.Load() != 0 {
					t.Fatal(got, starts.Load())
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("child effect", err)
				}
				if mode == "expired" && !errorCode(got.err, "permit_required") {
					t.Fatal(got.err)
				}
				if mode == "workspace_changed" && !errorCode(got.err, "source_changed") {
					t.Fatal(got.err)
				}
			}
			r.mu.Lock()
			remaining := r.permit
			r.mu.Unlock()
			if remaining != nil {
				t.Fatal("consumed permit restored")
			}
			if mode == "expired" {
				elapsed.Store(0)
				capture, err := r.Review(ctx, f.workspace)
				if !errorCode(err, "permit_required") || capture.Started {
					t.Fatal("permit reusable", capture, err)
				}
			}
		})
	}
}
