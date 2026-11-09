package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestOperationListIndexBackupVersionsRemainReadOnly(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		t.Run(version, func(t *testing.T) {
			store, dir := historyInventoryFixture(t)
			if err := reviewexec.EnsureSchema(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if version == "1" {
				if err := store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `DROP INDEX review_operations_listing`); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `UPDATE metadata SET value='1' WHERE key='schema:review_execution_indexes'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := backupRows(t, store.DB())
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, backupRows(t, store.DB())) {
				t.Fatal("backup validation changed source")
			}
			var retained string
			if err := store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution_indexes'`).Scan(&retained); err != nil || retained != version {
				t.Fatal("backup inspection migrated source", retained, err)
			}
		})
	}
}
