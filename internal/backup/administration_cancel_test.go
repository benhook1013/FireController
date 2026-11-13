package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

// Intercept only the selected semantic query, leaving schema reads untouched.
type administrationCancellationReader struct {
	sqlReader
	cancel context.CancelFunc
	phase  string
	once   sync.Once
	fired  bool
}

func (r *administrationCancellationReader) interrupt(query string) {
	match := r.phase == "project_management" && strings.HasPrefix(query, "SELECT COUNT(*) FROM grants WHERE typeof(can_manage)") || r.phase == "worker_retirements" && strings.Contains(query, "FROM worker_retirements r LEFT JOIN workers")
	match = match || r.phase == "grants_schema" && strings.Contains(query, "FROM sqlite_schema WHERE type='table' AND name='grants'")
	if match {
		r.once.Do(func() { r.fired = true; r.cancel() })
	}
}
func (r *administrationCancellationReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.interrupt(query)
	return r.sqlReader.QueryRowContext(ctx, query, args...)
}
func (r *administrationCancellationReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if r.phase == "retirement_schema" && strings.Contains(query, "FROM sqlite_schema") {
		for _, arg := range args {
			if arg == "worker_retirements" {
				r.once.Do(func() { r.fired = true; r.cancel() })
			}
		}
	}
	r.interrupt(query)
	return r.sqlReader.QueryContext(ctx, query, args...)
}

func TestAdministrationQueryCancellationIsUnavailableAndRetryable(t *testing.T) {
	for _, phase := range []string{"project_management", "worker_retirements"} {
		t.Run(phase, func(t *testing.T) {
			store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "source"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			versions, err := validateSchema(t.Context(), store.DB(), false)
			if err != nil {
				t.Fatal(err)
			}
			before := backupRows(t, store.DB())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &administrationCancellationReader{sqlReader: store.DB(), cancel: cancel, phase: phase}
			err = validateAdministrationContent(ctx, reader, versions)
			if !reader.fired || !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("selected SQL query was not interrupted", err)
			}
			backupCode(t, err, "backup_unavailable")
			if !reflect.DeepEqual(before, backupRows(t, store.DB())) {
				t.Fatal("validation changed source")
			}
			if err := validateAdministrationContent(t.Context(), store.DB(), versions); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}

// Intercept database/sql's context check in the real semantic validator,
// rather than canceling before the operation starts. Stack selection avoids
// production hooks, sleeps, and dependence on query timing.
type administrationOperationCancellation struct {
	context.Context
	cancel    context.CancelFunc
	phase     string
	operation string
	target    string
	once      sync.Once
	fired     atomic.Bool
	copied    atomic.Bool
}

func (c *administrationOperationCancellation) Done() <-chan struct{} {
	if c.Context.Err() == nil {
		var pcs [64]uintptr
		n := runtime.Callers(2, pcs[:])
		frames := runtime.CallersFrames(pcs[:n])
		administration, retirement, sqlQuery, inspecting := false, false, false, false
		for {
			frame, more := frames.Next()
			administration = administration || frame.Function == "github.com/benhook1013/FireController/internal/backup.validateAdministrationContent"
			retirement = retirement || frame.Function == "github.com/benhook1013/FireController/internal/core.ValidateWorkerRetirementsSnapshot"
			sqlQuery = sqlQuery || frame.Function == "database/sql.(*Tx).QueryContext"
			inspecting = inspecting || frame.Function == "github.com/benhook1013/FireController/internal/backup.inspectManifest"
			if !more {
				break
			}
		}
		selected := administration && sqlQuery && ((c.phase == "worker_retirements") == retirement)
		if c.operation == "restore_post_copy" {
			selected = selected && !inspecting
		}
		if selected {
			c.once.Do(func() {
				if info, err := os.Stat(filepath.Join(c.target, "firecontroller.sqlite")); err == nil && info.Size() > 0 {
					c.copied.Store(true)
				}
				c.fired.Store(true)
				c.cancel()
			})
		}
	}
	return c.Context.Done()
}

func TestAdministrationOperationCancellationIsUnavailableAndRetryable(t *testing.T) {
	for _, operation := range []string{"inspect", "restore", "restore_post_copy", "create", "managed_create"} {
		for _, phase := range []string{"project_management", "worker_retirements"} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
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
					case "restore", "restore_post_copy":
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
				ctx := &administrationOperationCancellation{Context: base, cancel: cancel, phase: phase, operation: operation, target: target}
				err = run(ctx)
				if !ctx.fired.Load() || !errors.Is(base.Err(), context.Canceled) {
					t.Fatal("operation context was not canceled", err)
				}
				backupCode(t, err, "backup_unavailable")
				if ctx.copied.Load() != (operation == "restore_post_copy") {
					t.Fatal("cancellation reached the wrong restore copy phase", ctx.copied.Load())
				}
				if operation == "create" || operation == "managed_create" {
					entries, err := os.ReadDir(target)
					if err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) != 0 {
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

func TestAdministrationSchemaCancellationIsUnavailableAndRetryable(t *testing.T) {
	for _, phase := range []string{"grants_schema", "retirement_schema"} {
		t.Run(phase, func(t *testing.T) {
			store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "source"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			before := backupRows(t, store.DB())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &administrationCancellationReader{sqlReader: store.DB(), cancel: cancel, phase: phase}
			_, err = validateSchema(ctx, reader, false)
			if !reader.fired || !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("selected schema query not interrupted", err)
			}
			backupCode(t, err, "backup_unavailable")
			if !reflect.DeepEqual(before, backupRows(t, store.DB())) {
				t.Fatal("schema validation changed source")
			}
			if _, err := validateSchema(t.Context(), store.DB(), false); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}
