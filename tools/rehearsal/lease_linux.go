//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLease opens an existing same-host inode. The service bind-mounts this
// exact lease into its RootDirectory; it must never create or replace the file.
// The static foreground runner launches no children and holds custody until exit.
func acquireLease(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("canonical lease required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return nil, errors.New("aliased lease")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0022 != 0 {
		return nil, errors.New("unsafe lease")
	}
	old, ok := before.Sys().(*syscall.Stat_t)
	if !ok || old.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("foreign lease")
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	after, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	now, ok := after.Sys().(*syscall.Stat_t)
	if !ok || old.Dev != now.Dev || old.Ino != now.Ino || !after.Mode().IsRegular() || now.Uid != uint32(os.Geteuid()) || after.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, errors.New("lease changed")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func requireLeaseIdentity(f *os.File, device, inode uint64) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || device == 0 || inode == 0 || uint64(s.Dev) != device || s.Ino != inode {
		return errors.New("lease identity differs")
	}
	return nil
}
