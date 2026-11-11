package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	match := r.phase == "project_management" && strings.HasPrefix(query, "SELECT COUNT(*) FROM grants WHERE typeof(can_manage)") || r.phase == "worker_retirements" && strings.HasPrefix(query, "SELECT r.retired_at,r.actor_id,w.id FROM worker_retirements")
	if match {
		r.once.Do(func() { r.fired = true; r.cancel() })
	}
}
func (r *administrationCancellationReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.interrupt(query)
	return r.sqlReader.QueryRowContext(ctx, query, args...)
}
func (r *administrationCancellationReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
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

func TestAdministrationOperationCancellationIsUnavailableAndRetryable(t *testing.T) {
	for _, operation := range []string{"inspect", "restore", "create", "managed_create"} {
		t.Run(operation, func(t *testing.T) {
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
			cancel()
			err = run(base)
			if !errors.Is(base.Err(), context.Canceled) {
				t.Fatal("operation context was not canceled", err)
			}
			backupCode(t, err, "backup_unavailable")
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
