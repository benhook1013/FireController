package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// Match the existing public-site cancellation fixture: interrupt database/sql's
// context check only inside the selected real validator, after earlier schema,
// integrity and reference validation. No timer or production hook is involved.
type historyScanCancellation struct {
	context.Context
	cancel context.CancelFunc
	phase  string
	fired  atomic.Bool
}

func (c *historyScanCancellation) Done() <-chan struct{} {
	if c.Context.Err() != nil {
		return c.Context.Done()
	}
	var pcs [40]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	selected, sqlCall := false, false
	for {
		frame, more := frames.Next()
		selected = selected || frame.Function == c.phase
		sqlCall = sqlCall || strings.HasPrefix(frame.Function, "database/sql.")
		if !more {
			break
		}
	}
	if selected && sqlCall {
		c.fired.Store(true)
		c.cancel()
	}
	return c.Context.Done()
}

func TestHistoryScanCancellationIsUnavailableAndReadOnly(t *testing.T) {
	f := comparisonInventoryFixture(t, true)
	rows := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	for _, phase := range []string{
		"reviews.ValidateHistoryReferences",
		"reviews.ValidateCurrentSummaries",
		"reviewexec.ValidateComparisonProofs",
	} {
		for _, operation := range []string{"inspect", "restore"} {
			t.Run(phase+"/"+operation, func(t *testing.T) {
				base, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx := &historyScanCancellation{Context: base, cancel: cancel, phase: "github.com/benhook1013/FireController/internal/" + phase}
				target := filepath.Join(t.TempDir(), "restored")
				var err error
				if operation == "inspect" {
					_, err = Inspect(ctx, bundle.Path)
				} else {
					_, err = Restore(ctx, bundle.Path, target)
				}
				if !ctx.fired.Load() || !errors.Is(base.Err(), context.Canceled) {
					t.Fatal("selected history validator was not interrupted", err)
				}
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != "backup_unavailable" || failure.Status != 503 {
					t.Error("history interruption lost retryable backup classification", err)
				}
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("interrupted verification created restore destination", err)
				}
				if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) || !reflect.DeepEqual(rows, backupRows(t, f.store.DB())) {
					t.Fatal("interrupted verification changed bundle or source rows")
				}
			})
		}
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("transient interruption was mistaken for persistent corruption", err)
	}
}
