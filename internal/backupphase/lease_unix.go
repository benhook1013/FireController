//go:build linux || darwin

package backupphase

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func tryLock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func busy(e error) bool        { return errors.Is(e, unix.EWOULDBLOCK) || errors.Is(e, unix.EAGAIN) }
