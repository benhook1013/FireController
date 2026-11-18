package backup

import (
	"github.com/benhook1013/FireController/internal/core"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func removeOperationalProofForLegacyFixture(t *testing.T, store *core.Store) {
	t.Helper()
	var columns int
	if err := store.DB().QueryRow("SELECT count(*) FROM pragma_table_info('review_finding_versions') WHERE name='operational_proof_json'").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns == 0 {
		return
	}
	for _, stmt := range []string{`DROP VIEW review_findings`, `ALTER TABLE review_finding_versions DROP COLUMN operational_proof_json`, reviews.HistoryReadViewSQLV12()["review_findings"]} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

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
			case "integrated12":
				statements = append(statements, `UPDATE metadata SET value='12' WHERE key='schema:reviews'`)
			case "slice11", "integrated11":
				statements = append(statements, `UPDATE metadata SET value='11' WHERE key='schema:reviews'`)
			}
			if lineage != "fresh" {
				removeOperationalProofForLegacyFixture(t, store)
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
			if err != nil || versions["reviews"] != 13 {
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
