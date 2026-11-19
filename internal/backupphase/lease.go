// Package backupphase coordinates cooperating backup storage producers.
package backupphase

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
)

var ErrInvalid = errors.New("invalid backup storage phase lease")

// ErrBusy means a nonblocking acquisition found an existing lease holder.
var ErrBusy = errors.New("backup storage phase lease is busy")

// Acquire requires a preprovisioned, private regular file. It never creates,
// removes or replaces the lock inode; recovery must retain this independent file.
// Callers hold no provider or filesystem mutation locks while waiting.
func Acquire(ctx context.Context, path string) (func(), error) {
	return acquire(ctx, path, true)
}

// TryAcquire attempts the same guarded lease once without waiting.
func TryAcquire(ctx context.Context, path string) (func(), error) {
	return acquire(ctx, path, false)
}

func acquire(ctx context.Context, path string, block bool) (func(), error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	if path == "" {
		return func() {}, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 2048 || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return nil, ErrInvalid
	}
	if !safeParents(path) {
		return nil, ErrInvalid
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalid
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, ErrInvalid
	}
	valid := func() bool {
		a, e := f.Stat()
		b, e2 := os.Lstat(path)
		return e == nil && e2 == nil && b.Mode().IsRegular() && os.SameFile(before, a) && os.SameFile(a, b)
	}
	if !valid() {
		f.Close()
		return nil, ErrInvalid
	}
	wait, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	for {
		if err := wait.Err(); err != nil {
			f.Close()
			return nil, err
		}
		if err := tryLock(f); err == nil {
			if !valid() || wait.Err() != nil {
				f.Close()
				if wait.Err() != nil {
					return nil, wait.Err()
				}
				return nil, ErrInvalid
			}
			return func() { f.Close() }, nil
		} else if !busy(err) {
			f.Close()
			return nil, ErrInvalid
		}
		if !block {
			f.Close()
			return nil, ErrBusy
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			f.Close()
			return nil, wait.Err()
		case <-timer.C:
		}
	}
}

// Parent custody prevents an unrelated writer from replacing the shared inode
// after acquisition. Sticky system ancestors are allowed, not symlink aliases.
func safeParents(path string) bool {
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return false
		}
		if filepath.Dir(p) == p {
			return true
		}
	}
}
