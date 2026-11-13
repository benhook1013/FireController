package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestActivityEvidenceSchemaBackupCompatibility(t *testing.T) {
	for _, version := range []string{"4", "5"} {
		t.Run(version, func(t *testing.T) {
			store, dir := historyInventoryFixture(t)
			if err := reviewexec.EnsureSchema(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if version == "4" {
				if err := store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `DROP TABLE review_activity_evidence; UPDATE metadata SET value='4' WHERE key='schema:review_evidence'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
		})
	}
}
