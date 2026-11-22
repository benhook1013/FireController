//go:build windows

package reviewruntime

import (
	"errors"
	"syscall"
)

func captureDiskFailureCode(err error) string {
	switch {
	case errors.Is(err, syscall.Errno(1816)): // ERROR_NOT_ENOUGH_QUOTA
		return "capture_disk_quota"
	case errors.Is(err, syscall.Errno(112)), errors.Is(err, syscall.Errno(39)): // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
		return "capture_disk_full"
	default:
		return ""
	}
}
