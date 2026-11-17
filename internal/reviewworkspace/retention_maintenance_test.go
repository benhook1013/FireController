package reviewworkspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPruningMaintenanceSharesDeadlineAfterPrune(t *testing.T) {
	f := newFixture(t)
	id := "review_attempt_" + strings.Repeat("d", 32)
	path := filepath.Join(f.root, "workspaces", id)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var original time.Time
	observed := make(chan error, 1)
	sentinel := errors.New("maintenance refused")
	done, err := f.manager.StartPruningWithMaintenance(ctx, func(c context.Context, _ string) (bool, error) {
		var ok bool
		original, ok = c.Deadline()
		if !ok {
			t.Error("missing prune deadline")
		}
		return true, nil
	}, func(err error) { observed <- err; cancel() }, func(c context.Context) error {
		deadline, ok := c.Deadline()
		if !ok || !deadline.Equal(original) {
			t.Error("maintenance extended original pass deadline")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("maintenance ran before completed prune", err)
		}
		return sentinel
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-observed:
		if !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no maintenance result")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestPruningMaintenanceRefusesFailedOrCanceledPrune(t *testing.T) {
	for _, mode := range []string{"failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			id := "review_attempt_" + strings.Repeat("e", 32)
			path := filepath.Join(f.root, "workspaces", id)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int64
			sentinel := errors.New("native custody unknown")
			done, err := f.manager.StartPruningWithMaintenance(ctx, func(context.Context, string) (bool, error) {
				if mode == "cancel" {
					cancel()
					return false, nil
				}
				return false, sentinel
			}, func(err error) {
				if !errors.Is(err, sentinel) {
					t.Error(err)
				}
				cancel()
			}, func(context.Context) error { calls.Add(1); return nil })
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not stop")
			}
			if calls.Load() != 0 {
				t.Fatal("maintenance admitted after failed prune")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("protected workspace changed", err)
			}
		})
	}
}

func TestPruningMaintenanceRetainsSingleWorkerAndCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done, err := f.manager.StartPruningWithMaintenance(ctx, func(context.Context, string) (bool, error) { return false, nil }, nil, func(c context.Context) error {
		close(entered)
		<-c.Done()
		return c.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not start")
	}
	if _, err := f.manager.StartPruning(ctx, func(context.Context, string) (bool, error) { return false, nil }, nil); err == nil {
		t.Fatal("second loop admitted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback cancellation lost")
	}
	// The buffered terminal value precedes deferred exclusion release.
	// Channel closure occurs after that defer and is the completion boundary.
	for range done {
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	next, err := f.manager.StartPruningWithMaintenance(stopped, func(context.Context, string) (bool, error) { return false, nil }, nil, func(context.Context) error { t.Error("canceled worker invoked callback"); return nil })
	if err != nil {
		t.Fatal("worker exclusion not released", err)
	}
	<-next
}
