//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package reviewruntime

import (
	"errors"
	"syscall"
)

func captureDiskFailureCode(err error) string {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return "capture_disk_full"
	case errors.Is(err, syscall.EDQUOT):
		return "capture_disk_quota"
	default:
		return ""
	}
}
