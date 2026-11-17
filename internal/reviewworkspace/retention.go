package reviewworkspace

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Prune removes only journal-authorized terminal attempt trees, in bounded
// batches. Renaming before deletion makes interrupted cleanup resumable without
// a second persistence model. os.Root confines even nested symlinks to this root.
// Shared Git caches and durable provider captures are outside this directory.
func (m *Manager) Prune(ctx context.Context, disposable func(context.Context, string) (bool, error)) (int, error) {
	if ctx == nil || disposable == nil || m == nil || m.layout() != nil {
		return 0, errConfiguration
	}
	root, err := os.OpenRoot(filepath.Join(m.root, "workspaces"))
	if err != nil {
		return 0, err
	}
	defer root.Close()
	removed := 0
	// Resume a partially removed tree before admitting more cleanup work.
	for _, resuming := range []bool{true, false} {
		dir, err := root.Open(".")
		if err != nil {
			return 0, err
		}
		defer dir.Close()
		for {
			if ctx.Err() != nil {
				return removed, ctx.Err()
			}
			entries, readErr := dir.ReadDir(64)
			for _, entry := range entries {
				name := entry.Name()
				if strings.HasPrefix(name, ".retired-") != resuming {
					continue
				}
				id := strings.TrimPrefix(name, ".retired-")
				if !strings.HasPrefix(id, "review_attempt_") || !lowerHex(strings.TrimPrefix(id, "review_attempt_"), 32) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				allowed, err := disposable(ctx, id)
				if err != nil {
					return removed, err
				}
				if !allowed {
					continue
				}
				before, err := root.Lstat(name)
				if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
					return removed, errVerification
				}
				retired := ".retired-" + id
				if name != retired {
					if _, err := root.Lstat(retired); !errors.Is(err, os.ErrNotExist) {
						return removed, errConflict
					}
					if err := root.Rename(name, retired); err != nil {
						return removed, err
					}
				}
				after, err := root.Lstat(retired)
				if err != nil || !os.SameFile(before, after) {
					return removed, errVerification
				}
				if err := removeTree(ctx, root, retired); err != nil {
					return removed, err
				}
				removed++
				if removed == 16 {
					return removed, nil
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return removed, readErr
			}
		}
	}
	return removed, nil
}

func removeTree(ctx context.Context, root *os.Root, path string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	info, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		child, err := root.OpenRoot(path)
		if err != nil {
			return err
		}
		err = removeChildren(ctx, child)
		child.Close()
		if err != nil {
			return err
		}
	}
	// A symlink is unlinked itself, never traversed.
	return root.Remove(path)
}

// Resolve each directory once. Re-resolving the full deep Git path for every
// file amplifies filesystem calls under container CPU limits.
func removeChildren(ctx context.Context, root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err := removeTree(ctx, root, entry.Name()); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// StartPruning owns one maintenance loop independent of review result polling.
func (m *Manager) StartPruning(ctx context.Context, disposable func(context.Context, string) (bool, error), report func(error)) (<-chan error, error) {
	if m == nil || ctx == nil || disposable == nil || !m.pruning.CompareAndSwap(false, true) {
		return nil, errConfiguration
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer m.pruning.Store(false)
		for ctx.Err() == nil {
			pass, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := m.Prune(pass, disposable)
			cancel()
			if err != nil && ctx.Err() == nil && report != nil {
				report(err)
			}
			timer := time.NewTimer(time.Minute)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		done <- nil
	}()
	return done, nil
}
