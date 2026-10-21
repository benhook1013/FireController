package backup

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholds"
)

func TestAdoptionVersionCanonicalBackupRestoreAndResealedNoncanonicalRefusal(t *testing.T) {
	f := newRestoreFixture(t)
	if err := adoptionholds.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	// Exercise an empty installed domain: logical traversal must not be needed
	// to enforce canonical version metadata, either locally or in a bundle.
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("canonical adoption version failed inspection", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-canonical-version")
	if restored, err := Restore(t.Context(), bundle.Path, destination); err != nil || restored.State != "inactive" {
		t.Fatal("canonical adoption version failed inactive restore", restored, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := adoptionholds.ValidateSnapshot(t.Context(), db); err != nil {
		db.Close()
		t.Fatal("restored canonical adoption domain refused", err)
	}
	after := backupRows(t, db)
	db.Close()
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && after[table] != rows {
			t.Fatalf("inactive restore changed retained table: %s", table)
		}
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("canonical backup/inspect/restore changed source or bundle")
	}
	// The manifest is resealed, so Inspect/Restore must reject the stored value,
	// not a stale database hash. Backup's generic version guard independently
	// enforces this boundary before the domain's logical snapshot validator.
	mutateBundleSQL(t, bundle.Path, `UPDATE metadata SET value='01' WHERE key='schema:review_adoption_obligations'`)
	assertCurrentSummaryInventoryRefused(t, bundle, false)
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("rejected noncanonical backup changed source")
	}
}
