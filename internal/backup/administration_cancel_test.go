package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// Interrupt a real database/sql call in the selected administration validator.
// With no recovery-history domain, the first QueryRowContext called directly
// by validateSnapshotContent is the management-grant query. Nested schema and
// connection checks remain uncanceled. No timer or production hook is needed.
type administrationValidationCancellation struct {
	context.Context
	cancel context.CancelFunc
	phase  string
	once   sync.Once
	fired  atomic.Bool
}

func (c *administrationValidationCancellation) Done() <-chan struct{} {
	if c.Context.Err() == nil {
		var pcs [48]uintptr
		n := runtime.Callers(2, pcs[:])
		frames := runtime.CallersFrames(pcs[:n])
		previous := ""
		for {
			frame, more := frames.Next()
			management := c.phase == "project_management" && frame.Function == "github.com/benhook1013/FireController/internal/backup.validateSnapshotContent" && previous == "database/sql.(*Tx).QueryRowContext"
			retirement := c.phase == "worker_retirements" && frame.Function == "github.com/benhook1013/FireController/internal/core.ValidateWorkerRetirementsSnapshot" && previous == "database/sql.(*Tx).QueryContext"
			if management || retirement {
				c.once.Do(func() { c.fired.Store(true); c.cancel() })
				break
			}
			if !more {
				break
			}
			previous = frame.Function
		}
	}
	return c.Context.Done()
}

func TestAdministrationValidationCancellationIsUnavailableAndRetryable(t *testing.T) {
	for _, phase := range []string{"project_management", "worker_retirements"} {
		for _, operation := range []string{"inspect", "restore", "create", "managed_create"} {
			t.Run(phase+"/"+operation, func(t *testing.T) {
				source := filepath.Join(t.TempDir(), "source")
				store, err := core.Open(t.Context(), source)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				versions, err := validateSchema(t.Context(), store.DB(), false)
				if err != nil || versions["project_management"] != 1 || versions["worker_retirements"] != 1 || versions["recovery_history"] != 0 {
					t.Fatal("fixture must select the two administration validators without recovery history", versions, err)
				}
				bundle, err := Create(t.Context(), store, source, filepath.Join(t.TempDir(), "bundles"))
				if err != nil {
					t.Fatal(err)
				}
				beforeBundle := restoreTree(t, bundle.Path)
				beforeSource := backupRows(t, store.DB())
				target := filepath.Join(t.TempDir(), "destination")
				run := func(ctx context.Context) error {
					switch operation {
					case "inspect":
						_, err := Inspect(ctx, bundle.Path)
						return err
					case "restore":
						_, err := Restore(ctx, bundle.Path, target)
						return err
					case "create":
						_, err := Create(ctx, store, source, target)
						return err
					default:
						_, err := createManaged(ctx, store, source, target)
						return err
					}
				}
				base, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx := &administrationValidationCancellation{Context: base, cancel: cancel, phase: phase}
				err = run(ctx)
				if !ctx.fired.Load() || !errors.Is(base.Err(), context.Canceled) {
					t.Fatal("did not interrupt the selected administration query", err)
				}
				backupCode(t, err, "backup_unavailable")
				if operation == "create" || operation == "managed_create" {
					entries, err := os.ReadDir(target)
					if err != nil || len(entries) != 0 {
						t.Fatal("interrupted validation retained an unpublished bundle", entries, err)
					}
				} else if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("interrupted verification created a restore destination", err)
				}
				if !reflect.DeepEqual(beforeBundle, restoreTree(t, bundle.Path)) || !reflect.DeepEqual(beforeSource, backupRows(t, store.DB())) {
					t.Fatal("interrupted validation changed the source or bundle")
				}
				if err := run(t.Context()); err != nil {
					t.Fatal("uncanceled retry failed", err)
				}
			})
		}
	}
}
