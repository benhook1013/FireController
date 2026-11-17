//go:build linux

package reviewruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
	"github.com/benhook1013/FireController/internal/strictjson"
)

var cacheSessionPattern = regexp.MustCompile(`^[0-9a-f]{8}/[0-9a-f]{8}/reviews/[0-9]{13}$`)

// vendorCacheBinding observes technical metadata only. It is not a deletion
// permit. Caller must supply the actual configured CLI HOME in this namespace,
// native settlement/capture proof, and participating/external producer custody.
type vendorCacheBinding struct {
	sessionDevice, sessionInode uint64
	metadataSHA256              string
	workspace                   string
}

func observeVendorCachePath(home, relative, attempt string, manager *reviewworkspace.Manager) (vendorCacheBinding, error) {
	var zero vendorCacheBinding
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || !cacheSessionPattern.MatchString(relative) {
		return zero, errors.New("cache path unavailable")
	}
	workspace, err := manager.RetiredPath(attempt)
	if err != nil {
		return zero, err
	}
	// Pin every component from / using no-follow opens. No ancestor symlink,
	// including a HOME alias, is accepted by this conservative observation.
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return zero, err
	}
	path := filepath.Join(home, ".coderabbit", "reviews", relative)
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if e != nil {
			return zero, e
		}
		fd = next
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		return zero, errors.New("cache ownership unavailable")
	}
	read := func(name string, limit int64) ([]byte, error) {
		child, e := syscall.Openat(fd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if e != nil {
			return nil, e
		}
		f := os.NewFile(uintptr(child), name)
		defer f.Close()
		var before syscall.Stat_t
		if syscall.Fstat(child, &before) != nil || before.Mode&syscall.S_IFMT != syscall.S_IFREG || before.Nlink != 1 || before.Uid != uint32(os.Geteuid()) || before.Mode&0022 != 0 || before.Size > limit {
			return nil, errors.New("cache metadata unavailable")
		}
		b, e := io.ReadAll(io.LimitReader(f, limit+1))
		if e != nil || int64(len(b)) > limit {
			return nil, errors.New("cache metadata limit")
		}
		var after syscall.Stat_t
		if syscall.Fstat(child, &after) != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
			return nil, errors.New("cache metadata changed")
		}
		return b, nil
	}
	metadata, err := read("git.json", 64<<10)
	if err != nil {
		return zero, err
	}
	if _, err = read(".session-complete-v2", 4096); err != nil {
		return zero, err
	}
	if strictjson.Validate(metadata) != nil {
		return zero, errors.New("cache metadata invalid")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(metadata, &fields) != nil {
		return zero, errors.New("cache metadata invalid")
	}
	var observed string
	if json.Unmarshal(fields["workingDirectory"], &observed) != nil || observed != workspace {
		return zero, errors.New("cache workspace mismatch")
	}
	sum := sha256.Sum256(metadata)
	return vendorCacheBinding{st.Dev, st.Ino, hex.EncodeToString(sum[:]), workspace}, nil
}
