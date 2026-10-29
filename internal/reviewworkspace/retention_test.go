package reviewworkspace_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneRetiresOnlyAuthorizedCheckoutsAndResumesInterruptedDeletion(t *testing.T) {
	f := newFixture(t)
	id := "review_attempt_" + strings.Repeat("a", 32)
	protected := "review_attempt_" + strings.Repeat("b", 32)
	for _, id := range []string{id, protected} {
		if _, err := f.manager.Prepare(t.Context(), 42, id, f.candidate, f.base); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "preserved.txt")
	write(t, outside, "preserve unrelated file", 0600)
	if err := os.Symlink(outside, filepath.Join(f.root, "workspaces", id, "nested-link")); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(f.root, "workspaces", "unknown-directory")
	if err := os.Mkdir(unknown, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	n, err := f.manager.Prune(ctx, func(_ context.Context, got string) (bool, error) {
		if got == id {
			cancel()
			return true, nil
		}
		return false, nil
	})
	if err == nil || n != 0 {
		t.Fatal("interruption not observed", n, err)
	}
	if _, err = os.Stat(filepath.Join(f.root, "workspaces", ".retired-"+id)); err != nil {
		t.Fatal("cleanup not recoverable", err)
	}
	n, err = f.manager.Prune(t.Context(), func(_ context.Context, got string) (bool, error) { return got == id, nil })
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	for _, path := range []string{outside, unknown, f.cache, filepath.Join(f.root, "workspaces", protected)} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal("unrelated/recoverable state removed", path, err)
		}
	}
	n, err = f.manager.Prune(t.Context(), func(context.Context, string) (bool, error) { return false, nil })
	if err != nil || n != 0 {
		t.Fatal("cleanup replay changed state", n, err)
	}
}

func TestPruneSkipsTopLevelSymlinksAndIsBounded(t *testing.T) {
	f := newFixture(t)
	external := t.TempDir()
	id := "review_attempt_" + strings.Repeat("f", 32)
	if err := os.Symlink(external, filepath.Join(f.root, "workspaces", id)); err != nil {
		t.Fatal(err)
	}
	if n, err := f.manager.Prune(t.Context(), func(context.Context, string) (bool, error) { t.Fatal("symlink admitted"); return true, nil }); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	for i := 0; i < 18; i++ {
		if err := os.Mkdir(filepath.Join(f.root, "workspaces", fmt.Sprintf("review_attempt_%032x", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := f.manager.Prune(t.Context(), func(context.Context, string) (bool, error) { return true, nil }); err != nil || n != 16 {
		t.Fatal("batch unbounded", n, err)
	}
	if n, err := f.manager.Prune(t.Context(), func(context.Context, string) (bool, error) { return true, nil }); err != nil || n != 2 {
		t.Fatal("next batch failed", n, err)
	}

}

func TestPruningWorkerIsSingleOwnedAndStopsOnCancellation(t *testing.T) {
	f := newFixture(t)
	id := "review_attempt_" + strings.Repeat("c", 32)
	if err := os.Mkdir(filepath.Join(f.root, "workspaces", id), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := make(chan struct{}, 1)
	eligible := func(context.Context, string) (bool, error) {
		select {
		case observed <- struct{}{}:
		default:
		}
		return false, nil
	}
	done, err := f.manager.StartPruning(ctx, eligible, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.StartPruning(ctx, eligible, nil); err == nil {
		t.Fatal("duplicate worker started")
	}
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("initial pass did not run")
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
	if _, err = os.Stat(filepath.Join(f.root, "workspaces", id)); err != nil {
		t.Fatal("protected attempt removed", err)
	}
}
