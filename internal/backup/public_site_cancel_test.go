package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
)

// Cancel only when the real public-site validator reaches database/sql's
// context check. Earlier manifest/schema/domain validation remains uncanceled;
// no production hook or timing race determines which branch is exercised.
type publicSiteValidationCancellation struct {
	context.Context
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (c *publicSiteValidationCancellation) Err() error {
	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.Function == "github.com/benhook1013/FireController/internal/publicsite.ValidateSnapshot" {
			c.fired.Store(true)
			c.cancel()
			break
		}
		if !more {
			break
		}
	}
	return c.Context.Err()
}

func TestPublicSiteValidationCancellationIsUnavailableAndReadOnly(t *testing.T) {
	f, _ := publicSiteRestoreFixture(t, true)
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	for _, operation := range []string{"inspect", "restore"} {
		t.Run(operation, func(t *testing.T) {
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &publicSiteValidationCancellation{Context: base, cancel: cancel}
			target := filepath.Join(t.TempDir(), "restored")
			var err error
			if operation == "inspect" {
				_, err = Inspect(ctx, bundle.Path)
			} else {
				_, err = Restore(ctx, bundle.Path, target)
			}
			if !ctx.fired.Load() || !errors.Is(base.Err(), context.Canceled) {
				t.Fatal("did not interrupt the public-site validator", err)
			}
			backupCode(t, err, "backup_unavailable")
			if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("interrupted verification created restore destination", err)
			}
			if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
				t.Fatal("interrupted verification changed source bundle")
			}
		})
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("cancellation was mistaken for lasting corruption", err)
	}
}
