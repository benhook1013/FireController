// Package capturecustody provides publication-wide capture custody on Linux.
// It never enumerates, reads or removes capture artifacts.
package capturecustody

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrUnavailable = errors.New("capture custody unavailable")
	ErrBusy        = errors.New("capture custody busy")
	ErrUnsupported = errors.New("capture custody unsupported")
)

const lockName = ".capture-publication.lock"

// Lease must span creation through publication, directory sync and this writer's
// staging removal. A collector must hold an exclusive lease for its entire work.
// Old writers do not participate; this cannot authorize collection of old files.
type Lease struct {
	root     string
	rootInfo os.FileInfo
	file     *os.File
	fileInfo os.FileInfo
	mu       sync.Mutex
	closed   bool
}

// Writer waits only within ctx, allowing concurrent writers. A deadline is
// mandatory so the prototype cannot accidentally create an unbounded wait.
func Writer(ctx context.Context, root string) (*Lease, error) {
	if ctx == nil {
		return nil, ErrUnavailable
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, ErrUnavailable
	}
	return acquire(ctx, root, false)
}

// Collector never waits for a writer. This grants custody, not deletion rights.
func Collector(ctx context.Context, root string) (*Lease, error) {
	if ctx == nil {
		return nil, ErrUnavailable
	}
	return acquire(ctx, root, true)
}

func acquire(ctx context.Context, root string, exclusive bool) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrUnavailable
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return nil, ErrUnavailable
	}
	ri, err := os.Lstat(root)
	if err != nil || !ri.IsDir() || ri.Mode().Perm() != 0700 || !owned(ri) {
		return nil, ErrUnavailable
	}
	f, err := openLock(filepath.Join(root, lockName))
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0600 || !owned(fi) {
		f.Close()
		return nil, ErrUnavailable
	}
	l := &Lease{root: root, rootInfo: ri, file: f, fileInfo: fi}
	success := false
	defer func() {
		if !success {
			l.Close()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := l.Validate(); err != nil {
			return nil, err
		}
		err = tryLock(f, exclusive)
		if err == nil {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if err = l.Validate(); err != nil {
				return nil, err
			}
			success = true
			return l, nil
		}
		if !errors.Is(err, ErrBusy) {
			return nil, err
		}
		if exclusive {
			return nil, ErrBusy
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Validate refuses changed paths/inodes/permissions. It is not protection against
// a hostile same-user process replacing names immediately after validation.
func (l *Lease) Validate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrUnavailable
	}
	canonical, err := filepath.EvalSymlinks(l.root)
	if err != nil || canonical != l.root {
		return ErrUnavailable
	}
	ri, err := os.Lstat(l.root)
	if err != nil || !os.SameFile(ri, l.rootInfo) || !ri.IsDir() || ri.Mode().Perm() != 0700 || !owned(ri) {
		return ErrUnavailable
	}
	fi, err := os.Lstat(filepath.Join(l.root, lockName))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0600 || !owned(fi) || !os.SameFile(fi, l.fileInfo) {
		return ErrUnavailable
	}
	return nil
}

func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.file.Close() // closing the independent open-file description releases flock
}
