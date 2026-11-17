//go:build linux

package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type vendorCacheCandidate struct{ Relative, Attempt string }

type vendorCacheInventory struct {
	Candidates []vendorCacheCandidate
	Skipped    int
}

// inventoryVendorCache reads only bounded technical metadata. Candidates are
// observations, not deletion authority; Retire repeats native and physical proof.
func inventoryVendorCache(ctx context.Context, home string, manager *reviewworkspace.Manager) (vendorCacheInventory, error) {
	if ctx == nil || manager == nil || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return vendorCacheInventory{}, errors.New("cache inventory unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return vendorCacheInventory{}, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Join(home, ".coderabbit", "reviews"), "/"), "/") {
		next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if e != nil {
			return vendorCacheInventory{}, e
		}
		fd = next
	}
	root := os.NewFile(uintptr(fd), "cache inventory")
	defer root.Close()
	hexName := regexp.MustCompile("^[0-9a-f]{8}$")
	stamp := regexp.MustCompile("^[0-9]{13}$")
	remaining := 4096
	var result vendorCacheInventory
	var walk func(*os.File, []string) error
	walk = func(dir *os.File, parts []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, err := dir.Readdirnames(remaining + 1)
		if err != nil && err != io.EOF {
			return err
		}
		if len(names) > remaining {
			return errors.New("cache inventory entry limit")
		}
		remaining -= len(names)
		slices.Sort(names)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			depth := len(parts)
			match := depth < 2 && hexName.MatchString(name) || depth == 2 && name == "reviews" || depth == 3 && stamp.MatchString(name)
			if !match {
				continue
			}
			child, e := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if e != nil {
				return e
			}
			sub := os.NewFile(uintptr(child), "cache inventory component")
			next := append(append([]string(nil), parts...), name)
			if depth < 3 {
				e = walk(sub, next)
			} else {
				var st syscall.Stat_t
				if syscall.Fstat(child, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
					e = errors.New("cache inventory ownership")
				} else {
					var attempt string
					attempt, e = inventoryCacheAttempt(child, manager)
					if e == nil && attempt != "" {
						relative := strings.Join(next, "/")
						if _, e = observeVendorCachePath(home, relative, attempt, manager); e == nil {
							result.Candidates = append(result.Candidates, vendorCacheCandidate{relative, attempt})

						}
					}
				}
			}
			sub.Close()
			if len(result.Candidates) > 256 {
				return errors.New("cache inventory candidate limit")
			}
			if depth == 3 && e != nil {
				result.Skipped++
				continue
			}
			if e != nil {
				return e
			}
		}
		return nil
	}
	if err := walk(root, nil); err != nil {
		return vendorCacheInventory{}, err
	}
	return result, nil
}

func inventoryCacheAttempt(fd int, manager *reviewworkspace.Manager) (string, error) {
	child, err := syscall.Openat(fd, "git.json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(child), "cache technical metadata")
	defer f.Close()
	var st syscall.Stat_t
	if syscall.Fstat(child, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Size > 64<<10 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		return "", errors.New("cache inventory metadata")
	}
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(b) > 64<<10 || strictjson.Validate(b) != nil {
		return "", errors.New("cache inventory metadata")
	}
	var fields map[string]json.RawMessage
	var workspace string
	if json.Unmarshal(b, &fields) != nil || json.Unmarshal(fields["workingDirectory"], &workspace) != nil || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return "", errors.New("cache inventory workspace")
	}
	attempt := filepath.Base(workspace)
	expected, err := manager.RetiredPath(attempt)
	if err != nil || expected != workspace {
		return "", nil
	}
	return attempt, nil
}
