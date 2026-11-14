//go:build linux

package backup

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

// Walk from the filesystem root with a retained directory descriptor. Neither
// ancestor aliases nor final-component symlinks are followed.
func openQuarantineDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, invalid()
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, invalid()
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, invalid()
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
func openQuarantineChild(parent *os.File, name string) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, invalid()
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, invalid()
	}
	return os.NewFile(uintptr(fd), name), nil
}
