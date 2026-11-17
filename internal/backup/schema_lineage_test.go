package backup

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewLineageUpgradesProduceCanonicalRestorableBackups(t *testing.T) {
	for _, lineage := range []string{"fresh", "main10", "slice11", "integrated10", "integrated11", "integrated12"} {
		t.Run(lineage, func(t *testing.T) {
			store, dir := historyInventoryFixture(t)
			historyInventorySeedPull(t, store)
			// Reviews' owner tests build genuine predecessors from their migration
			// sequences. Here recreate those same physical shapes around retained data
			// to exercise the independent backup inventory and inactive restore.
			var statements []string
			if lineage == "main10" || lineage == "slice11" || lineage == "integrated10" {
				statements = append(statements, `DROP VIEW review_pulls`, `ALTER TABLE review_pull_versions DROP COLUMN delivery_selected`, reviews.HistoryReadViewSQLV10()["review_pulls"])
			}
			if lineage == "main10" {
				statements = append(statements, `DROP INDEX review_run_versions_external_source`, `DROP TABLE review_hosted_discovery_state`)
			}
			switch lineage {
			case "main10", "integrated10":
				statements = append(statements, `UPDATE metadata SET value='10' WHERE key='schema:reviews'`)
			case "slice11", "integrated11":
				statements = append(statements, `UPDATE metadata SET value='11' WHERE key='schema:reviews'`)
			}
			for _, q := range statements {
				if _, err := store.DB().Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if err := reviews.EnsureSchema(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			versions, err := validateSchema(t.Context(), store.DB(), false)
			if err != nil || versions["reviews"] != 12 {
				t.Fatal("upgraded schema failed independent inventory", versions, err)
			}
			original := backupRows(t, store.DB())
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			restored, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil || restored.State != "inactive" {
				t.Fatal(restored, err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			retained := backupRows(t, db)
			for _, table := range []string{"review_snapshots", "review_workspace", "review_pull_versions", "review_pull_memberships", "review_current_pulls", "review_hosted_discovery_state"} {
				if retained[table] != original[table] {
					t.Fatal("restoration changed", table)
				}
			}
			if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("inspection/restore changed bundle")
			}
		})
	}
}
