package backup

import (
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestChannelRunCountsCanonicalSchema(t *testing.T) {
	store, _ := historyInventoryFixture(t)
	var actual string
	if err := store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_current_totals'`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	for _, s := range reviews.CurrentSummarySchemaSQL() {
		if strings.HasPrefix(s, "CREATE TABLE review_current_totals ") && historyDefinitionSQL(s) != historyDefinitionSQL(actual) {
			t.Fatalf("totals DDL mismatch\nactual %s\nexpected %s", actual, s)
		}
	}
	if err := validateHistoryPhysicalSchema(t.Context(), store.DB(), 11); err != nil {
		t.Fatal("physical", err)
	}
	if err := validateHistoryViews(t.Context(), store.DB(), 11); err != nil {
		t.Fatal("views", err)
	}
	if _, err := validateSchema(t.Context(), store.DB(), false); err != nil {
		t.Fatal("inventory", err)
	}
}

func TestChannelRunCountsCanonicalBackupRestoreAndResealedCorruption(t *testing.T) {
	f := currentSummaryInventoryFixture(t)
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	immutable := restoreTree(t, bundle.Path)
	destination := filepath.Join(t.TempDir(), "inactive-counts")
	restored, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != "inactive" {
		t.Fatal(restored)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && after[table] != rows {
			t.Fatal("restore changed retained rows", table)
		}
	}
	var cli, hosted int
	if err = db.QueryRow(`SELECT cli_run_count,hosted_run_count FROM review_current_totals WHERE pull_number=7`).Scan(&cli, &hosted); err != nil || cli != 1 || hosted != 0 {
		t.Fatal("exact restored counts", cli, hosted, err)
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("inactive restore rewrote source bundle")
	}
	// Same aggregate and valid FKs/DDL; only authoritative per-channel validation
	// can reject this canonically rehashed redistribution.
	mutateBundleSQL(t, bundle.Path, `UPDATE review_current_totals SET cli_run_count=0,hosted_run_count=1 WHERE pull_number=7`)
	assertCurrentSummaryInventoryRefused(t, bundle, true)
}

func TestChannelRunCountsAuthenticSchemaEightBackupRemainsSupported(t *testing.T) {
	testAuthenticChannelCountSchemaBackup(t, 8)
}

func testAuthenticChannelCountSchemaBackup(t *testing.T, version int) {
	dir := filepath.Join(t.TempDir(), "schema-eight")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = installLegacyReviewInventorySchema(t.Context(), store, 3); err != nil {
		t.Fatal(err)
	}
	statements := []string{`DROP TRIGGER review_runs_snapshot_revision_insert`, `DROP TRIGGER review_runs_snapshot_revision_update`, `DROP TABLE review_findings`, `DROP TABLE review_attempts`, `DROP TABLE review_runs`, `DROP TABLE review_retired_pulls`, `DROP TABLE review_pulls`}
	statements = append(statements, reviews.HistorySchemaSQLV10()...)
	if version == 8 {
		statements = append(statements, reviews.CurrentSummarySchemaSQLV8()...)
	} else {
		statements = append(statements, reviews.CurrentSummarySchemaSQL()...)
	}
	statements = append(statements, reviews.CurrentRunOrderSchemaSQL()...)
	for _, view := range reviews.HistoryReadViewSQLV10() {
		statements = append(statements, view)
	}
	migrations := make([][]string, version)
	migrations[3] = statements
	if err = store.EnsureSchema(t.Context(), "reviews", migrations...); err != nil {
		t.Fatal(err)
	}
	if _, err = validateSchema(t.Context(), store.DB(), false); err != nil {
		t.Fatal("authentic old schema", err)
	}
	before := backupRows(t, store.DB())
	bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-old-eight")
	if _, err = Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if versions, e := validateSchema(t.Context(), db, true); e != nil || versions["reviews"] != version {
		t.Fatal("inspection/restore silently migrated old schema", versions, e)
	}
	if !reflect.DeepEqual(before, backupRows(t, store.DB())) {
		t.Fatal("old backup rewrote source")
	}
}

// Tests that reconstruct genuine pre-nine bundles must remove the future
// derived columns too, not merely lower their version metadata.
func removeChannelRunCountsForLegacyFixture(t *testing.T, store *core.Store) {
	t.Helper()
	var selected int
	if err := store.DB().QueryRow(`SELECT count(*) FROM pragma_table_info('review_pull_versions') WHERE name='delivery_selected'`).Scan(&selected); err != nil {
		t.Fatal(err)
	}
	if selected != 0 {
		for _, statement := range []string{`DROP VIEW review_pulls`, `ALTER TABLE review_pull_versions DROP COLUMN delivery_selected`, reviews.HistoryReadViewSQLV10()["review_pulls"]} {
			if _, err := store.DB().Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, err := store.DB().Exec(`DROP INDEX IF EXISTS review_run_versions_external_source; DROP TABLE IF EXISTS review_hosted_discovery_state`); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := store.DB().QueryRow(`SELECT count(*) FROM pragma_table_info('review_current_totals') WHERE name='hosted_run_count'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns == 0 {
		return
	}
	for _, name := range []string{"hosted_run_count", "cli_run_count", "subagent_run_count", "manual_run_count"} {
		if _, err := store.DB().Exec(`ALTER TABLE review_current_totals DROP COLUMN ` + name); err != nil {
			t.Fatal(err)
		}
	}
}
