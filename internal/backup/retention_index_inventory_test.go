package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func retentionInventoryIndexNames() []string {
	var names []string
	for _, kind := range []string{"pull", "run", "attempt", "retired"} {
		names = append(names, "review_"+kind+"_versions_snapshot", "review_"+kind+"_memberships_snapshot_start", "review_"+kind+"_memberships_snapshot_end")
	}
	return names
}

func assertRetentionIndexInventory(t *testing.T, db *sql.DB, installed bool) {
	t.Helper()
	var version string
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "13" {
		t.Fatal("retention indexes must not change reviews v13", version, err)
	}
	want := 0
	if installed {
		want = 1
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM metadata WHERE key='schema:review_retention_indexes'`).Scan(&count); err != nil || count != want {
		t.Fatal("unexpected retention domain presence", count, err)
	}
	if installed {
		if err := db.QueryRow(`SELECT value FROM metadata WHERE key='schema:review_retention_indexes'`).Scan(&version); err != nil || version != "1" {
			t.Fatal("unexpected retention domain version", version, err)
		}
	}
	for _, name := range retentionInventoryIndexNames() {
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name=?`, name).Scan(&count); err != nil || count != want {
			t.Fatal("unexpected retention index presence", name, count, err)
		}
	}
}

func TestRetentionIndexInventoryCurrentAndPredecessor(t *testing.T) {
	for _, predecessor := range []bool{false, true} {
		name := "current"
		if predecessor {
			name = "pre-index-v13"
		}
		t.Run(name, func(t *testing.T) {
			store, dir := historyInventoryFixture(t)
			assertRetentionIndexInventory(t, store.DB(), true)
			if predecessor {
				// This optional domain only adds indexes: removing all twelve and
				// its stamp reconstructs the unchanged predecessor data format.
				var drops []string
				for _, index := range retentionInventoryIndexNames() {
					drops = append(drops, "DROP INDEX "+index)
				}
				drops = append(drops, `DELETE FROM metadata WHERE key='schema:review_retention_indexes'`)
				if _, err := store.DB().Exec(strings.Join(drops, ";")); err != nil {
					t.Fatal(err)
				}
			}
			before := backupRows(t, store.DB())
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			result, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil || result.State != "inactive" {
				t.Fatal(result, err)
			}
			for _, root := range []string{bundle.Path, destination} {
				db, err := openSnapshot(t.Context(), filepath.Join(root, "firecontroller.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				assertRetentionIndexInventory(t, db, !predecessor)
				db.Close()
			}
			assertRetentionIndexInventory(t, store.DB(), !predecessor)
			if !reflect.DeepEqual(before, backupRows(t, store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("backup validation migrated source or bundle")
			}
		})
	}
}

func TestRetentionIndexInventoryRejectsRehashedTampering(t *testing.T) {
	store, dir := historyInventoryFixture(t)
	assertRetentionIndexInventory(t, store.DB(), true)
	changes := []struct{ name, sql string }{
		{"wrong-index", `DROP INDEX review_run_memberships_snapshot_end; CREATE INDEX review_run_memberships_snapshot_end ON review_run_memberships(end_revision,project_id)`},
		{"indexes-without-stamp", `DELETE FROM metadata WHERE key='schema:review_retention_indexes'`},
		{"unsupported-version", `UPDATE metadata SET value='2' WHERE key='schema:review_retention_indexes'`},
	}
	for _, name := range retentionInventoryIndexNames() {
		changes = append(changes, struct{ name, sql string }{"missing-" + name, "DROP INDEX " + name})
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			bundle, err := Create(t.Context(), store, dir, filepath.Join(t.TempDir(), "bundles"))
			if err != nil {
				t.Fatal(err)
			}
			mutateBundleSQL(t, bundle.Path, change.sql) // Rehash database and manifest.
			immutable := restoreTree(t, bundle.Path)
			_, err = Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
			destination := filepath.Join(t.TempDir(), "must-not-exist")
			_, err = Restore(t.Context(), bundle.Path, destination)
			backupCode(t, err, "backup_invalid")
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("invalid index inventory reached restore writes", err)
			}
			if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("rejection changed bundle")
			}
		})
	}
}
