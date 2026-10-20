package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestReviewOrderInventoryCurrentSchema(t *testing.T) {
	f := newRestoreFixture(t)
	if err := validateReviewOrderPhysicalSchema(t.Context(), f.store.DB()); err != nil {
		t.Fatal("current review order schema", err)
	}
	if err := validateReviewOrderRows(t.Context(), f.store.DB()); err != nil {
		t.Fatal("current review order rows", err)
	}
}

func reviewOrderFixture(t *testing.T, allow bool) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	if !allow {
		return f
	}
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Enqueue(t.Context(), routePolicyInventoryScope(t, f), "explicit-order-backup", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40), AllowOutOfOrder: true}); err != nil {
		t.Fatal("enqueue explicit operation", err)
	}
	return f
}

func TestReviewOrderInventoryRoundTripRetainsExplicitChoice(t *testing.T) {
	f := reviewOrderFixture(t, true)
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-explicit-order")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var explicit int
	if err = db.QueryRow(`SELECT allow_out_of_order FROM review_operations WHERE allow_out_of_order=1`).Scan(&explicit); err != nil || explicit != 1 {
		t.Fatal("explicit order choice not retained", explicit, err)
	}
	retained := backupRows(t, db)
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && retained[table] != rows {
			t.Fatalf("restore changed %s", table)
		}
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("backup round trip changed source or sealed bundle")
	}
}

func TestReviewOrderInventoryAuthenticV6RemainsSupported(t *testing.T) {
	f := reviewOrderFixture(t, false)
	if _, err := f.store.DB().Exec(`ALTER TABLE review_operations DROP COLUMN allow_out_of_order; UPDATE metadata SET value='6' WHERE key='schema:review_execution'`); err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("genuine v6 refused", err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-v6")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM pragma_table_xinfo('review_operations') WHERE name='allow_out_of_order'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("v6 was migrated during restore", count, err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("v6 backup operations changed source or bundle")
	}
}

func TestReviewOrderInventoryAutomaticSelectionChildRoundTrip(t *testing.T) {
	f, selectionID := selectionInventoryFixture(t, "selected")
	var explicit int
	if err := f.store.DB().QueryRow(`SELECT o.allow_out_of_order FROM review_selections s JOIN review_operations o ON o.id=s.operation_id WHERE s.id=?`, selectionID).Scan(&explicit); err != nil || explicit != 0 {
		t.Fatal("automatic child gained caller-only explicit-order intent", explicit, err)
	}
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "inactive-automatic-child")
	if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.QueryRow(`SELECT o.allow_out_of_order FROM review_selections s JOIN review_operations o ON o.id=s.operation_id WHERE s.id=?`, selectionID).Scan(&explicit); err != nil || explicit != 0 {
		t.Fatal("restored automatic child gained caller-only explicit-order intent", explicit, err)
	}
}

func TestReviewOrderInventoryRejectsForgedAutomaticSelectionChild(t *testing.T) {
	f, selectionID := selectionInventoryFixture(t, "selected")
	bundle := restoreBundle(t, f)
	mutateBundleSQL(t, bundle.Path, `UPDATE review_operations SET allow_out_of_order=1 WHERE id=(SELECT operation_id FROM review_selections WHERE id=?)`, selectionID)
	assertCurrentSummaryInventoryRefused(t, bundle, true)
}

func TestReviewOrderInventoryRejectsResealedInvalidStorage(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"text", `'bad'`},
		{"blob", `x'01'`},
		{"real", `0.5`},
		{"out-of-range", `2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := reviewOrderFixture(t, true)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, `PRAGMA ignore_check_constraints=ON; UPDATE review_operations SET allow_out_of_order=`+tc.value+` WHERE allow_out_of_order=1`)
			assertCurrentSummaryInventoryRefused(t, bundle, true)
		})
	}
}

func TestReviewOrderInventoryRejectsResealedInvalidColumnContract(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"type", "allow_out_of_order INTEGER", "allow_out_of_order TEXT"},
		{"nullable", "allow_out_of_order INTEGER NOT NULL", "allow_out_of_order INTEGER"},
		{"default", "DEFAULT 0", "DEFAULT 1"},
		{"check", " CHECK(allow_out_of_order IN (0,1))", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := reviewOrderFixture(t, false)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,?,?) WHERE type='table' AND name='review_operations'; PRAGMA writable_schema=OFF`, tc.old, tc.replacement)
			assertCurrentSummaryInventoryRefused(t, bundle, false)
		})
	}
}

func TestReviewOrderInventoryRejectsResealedNullStorage(t *testing.T) {
	f := reviewOrderFixture(t, false)
	bundle := restoreBundle(t, f)
	const canonical = "allow_out_of_order INTEGER NOT NULL DEFAULT 0"
	const nullable = "allow_out_of_order INTEGER DEFAULT 0"
	mutateBundleSQL(t, bundle.Path, `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,?,?) WHERE type='table' AND name='review_operations'; PRAGMA writable_schema=OFF`, canonical, nullable)
	mutateBundleSQL(t, bundle.Path, `UPDATE review_operations SET allow_out_of_order=NULL`)
	mutateBundleSQL(t, bundle.Path, `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,?,?) WHERE type='table' AND name='review_operations'; PRAGMA writable_schema=OFF`, nullable, canonical)
	assertCurrentSummaryInventoryRefused(t, bundle, true)
	if _, err := os.Lstat(filepath.Join(bundle.Path, "firecontroller.sqlite")); err != nil {
		t.Fatal(err)
	}
}
