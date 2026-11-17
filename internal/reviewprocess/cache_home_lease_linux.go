//go:build linux

package reviewprocess

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// CacheHomeLease pins one private HOME lock inode. Only participating binaries
// honor it: initial adoption must prove prior/unmanaged HOME producers absent.
// Child fd inheritance preserves custody across supervising-daemon death for
// cooperating descendants that retain inherited descriptors. It is not a sandbox.
type CacheHomeLease struct {
	home          string
	directory     *os.File
	device, inode uint64
}

func NewCacheHomeLease(home string) (*CacheHomeLease, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return nil, errors.New("cache HOME invalid")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(home, "/"), "/") {
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	directory := os.NewFile(uintptr(fd), "CLI HOME")
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		directory.Close()
		return nil, errors.New("cache HOME ownership unavailable")
	}
	child, err := syscall.Openat(fd, ".firecontroller-cache.lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		directory.Close()
		return nil, err
	}
	defer syscall.Close(child)
	if syscall.Fstat(child, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		directory.Close()
		return nil, errors.New("cache HOME lease invalid")
	}
	return &CacheHomeLease{home, directory, st.Dev, st.Ino}, nil
}

func (l *CacheHomeLease) Close() error {
	if l == nil || l.directory == nil {
		return nil
	}
	return l.directory.Close()
}

func (l *CacheHomeLease) acquire(exclusive bool) (*os.File, error) {
	return l.acquireObserved(exclusive, nil)
}

func (l *CacheHomeLease) acquireObserved(exclusive bool, beforeLock func()) (*os.File, error) {
	if l == nil || l.directory == nil {
		return nil, errors.New("cache HOME lease unavailable")
	}
	fd, err := syscall.Openat(int(l.directory.Fd()), ".firecontroller-cache.lock", syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "CLI HOME lease")
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Dev != l.device || st.Ino != l.inode || st.Nlink != 1 || st.Mode&0777 != 0600 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, errors.New("cache HOME lease changed")
	}
	if beforeLock != nil {
		beforeLock()
	}
	op := syscall.LOCK_SH
	if exclusive {
		op = syscall.LOCK_EX
	}
	if err = syscall.Flock(fd, op|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errCacheHomeBusy
		}
		return nil, err
	}
	// Reopen the exact pathname after acquisition, not merely the pinned HOME.
	currentFD, e := syscall.Openat(int(l.directory.Fd()), ".firecontroller-cache.lock", syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		f.Close()
		return nil, e
	}
	var currentLock syscall.Stat_t
	e = syscall.Fstat(currentFD, &currentLock)
	syscall.Close(currentFD)
	if e != nil || currentLock.Mode&syscall.S_IFMT != syscall.S_IFREG || currentLock.Dev != l.device || currentLock.Ino != l.inode || currentLock.Nlink != 1 || currentLock.Mode&0777 != 0600 || currentLock.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, errors.New("cache HOME lease replaced during acquisition")
	}
	// The private HOME is installation-owned; refuse a changed path/alias rather
	// than silently joining a new lock generation.
	current, err := os.Lstat(l.home)
	pinned, e := l.directory.Stat()
	if err != nil || e != nil || !os.SameFile(current, pinned) || current.Mode()&os.ModeSymlink != 0 {
		f.Close()
		return nil, errors.New("cache HOME changed")
	}
	return f, nil
}
