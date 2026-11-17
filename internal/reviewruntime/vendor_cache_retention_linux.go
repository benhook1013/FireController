//go:build linux

package reviewruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

type VendorCacheRetirement struct {
	Files        int
	LogicalBytes int64
}

type vendorCacheRetirement = VendorCacheRetirement

// retireVendorCachePayloads removes only the two disposable vendor payloads of
// one native completed, captured and retired attempt. Metadata and all native
// evidence remain. It is not scheduled or enabled by the daemon. Adoption also
// requires operator-established exclusive ownership of the configured CLI HOME;
// this process gate cannot fence independently launched vendor processes.
func retireVendorCachePayloads(ctx context.Context, runner *reviewprocess.Runner, manager *reviewworkspace.Manager, engine *reviewexec.Engine, captures *CaptureStore, capability, relative, attempt string, limit int64, now time.Time) (vendorCacheRetirement, error) {
	return retireVendorCachePayloadsObserved(ctx, runner, manager, engine, captures, capability, relative, attempt, limit, now, nil)
}

// afterRemove is an internal fault-observation seam; production supplies nil.
func retireVendorCachePayloadsObserved(ctx context.Context, runner *reviewprocess.Runner, manager *reviewworkspace.Manager, engine *reviewexec.Engine, captures *CaptureStore, capability, relative, attempt string, limit int64, now time.Time, afterRemove func(int) error) (vendorCacheRetirement, error) {
	if runner == nil {
		return vendorCacheRetirement{}, errors.New("cache retirement unavailable")
	}
	return retireVendorCachePayloadsWithCustody(ctx, runner, manager, engine, captures, capability, relative, attempt, limit, now, afterRemove, runner.WithCacheMaintenance)
}

// The unqualified entry is private fixture plumbing. Operational callers use
// the adopted maintainer, whose custody callback checks the root binding twice.
func retireVendorCachePayloadsWithCustody(ctx context.Context, runner *reviewprocess.Runner, manager *reviewworkspace.Manager, engine *reviewexec.Engine, captures *CaptureStore, capability, relative, attempt string, limit int64, now time.Time, afterRemove func(int) error, maintenance func(context.Context, func(string) error) error) (vendorCacheRetirement, error) {
	var result vendorCacheRetirement
	if ctx == nil || runner == nil || manager == nil || engine == nil || captures == nil || maintenance == nil || limit <= 0 || limit > 256<<20 {
		return result, errors.New("cache retirement unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := maintenance(ctx, func(home string) error {
		verify := func() (reviewexec.SettledCLICapture, error) {
			observed, err := engine.ObserveSettledCLICapture(ctx, capability, attempt, now)
			if err != nil {
				return observed, err
			}
			if observed.AttemptID == "" {
				return observed, errors.New("cache attempt unsettled")
			}
			record, ref, err := captures.Read(ctx, attempt)
			if err != nil {
				return observed, err
			}
			if !record.Capture.Started || record.Capture.ExitCode != 0 || ref.ID != observed.CaptureID || ref.SHA256 != observed.SHA256 || ref.Bytes != observed.Bytes {
				return observed, errors.New("cache capture mismatch")
			}
			return observed, nil
		}
		proof, err := verify()
		if err != nil {
			return err
		}
		binding, err := observeVendorCachePath(home, relative, attempt, manager)
		if err != nil {
			return err
		}
		fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		for _, part := range strings.Split(strings.TrimPrefix(filepath.Join(home, ".coderabbit", "reviews", relative), "/"), "/") {
			next, e := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			syscall.Close(fd)
			if e != nil {
				return e
			}
			fd = next
		}
		defer syscall.Close(fd)
		var directory syscall.Stat_t
		if syscall.Fstat(fd, &directory) != nil || directory.Dev != binding.sessionDevice || directory.Ino != binding.sessionInode {
			return errors.New("cache session changed")
		}
		type payload struct {
			name string
			info syscall.Stat_t
		}
		files := []payload{}
		var total int64
		for _, name := range []string{"incrementalDiff.v2.json", "internalState.json"} {
			child, e := syscall.Openat(fd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if errors.Is(e, syscall.ENOENT) {
				continue
			}
			if e != nil {
				return e
			}
			var st syscall.Stat_t
			e = syscall.Fstat(child, &st)
			syscall.Close(child)
			if e != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 || st.Size < 0 || st.Size > limit-total {
				return errors.New("cache payload unavailable")
			}
			total += st.Size
			files = append(files, payload{name, st})
		}
		for _, file := range files {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fresh, e := verify()
			if e != nil {
				return e
			}
			if fresh != proof {
				return errors.New("cache settlement changed")
			}
			current, e := observeVendorCachePath(home, relative, attempt, manager)
			if e != nil {
				return e
			}
			if current != binding {
				return errors.New("cache binding changed")
			}
			child, e := syscall.Openat(fd, file.name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if e != nil {
				return e
			}
			var st syscall.Stat_t
			e = syscall.Fstat(child, &st)
			syscall.Close(child)
			if e != nil || st.Dev != file.info.Dev || st.Ino != file.info.Ino || st.Size != file.info.Size || st.Mtim != file.info.Mtim || st.Ctim != file.info.Ctim {
				return errors.New("cache payload changed")
			}
			if e = syscall.Unlinkat(fd, file.name); e != nil {
				return e
			}
			result.Files++
			result.LogicalBytes += st.Size
			if e = syscall.Fsync(fd); e != nil {
				return e
			}
			if afterRemove != nil {
				if e = afterRemove(result.Files); e != nil {
					return e
				}
			}
		}
		return nil
	})
	return result, err
}
