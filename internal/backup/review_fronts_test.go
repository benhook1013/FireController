package backup

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestFrontInventoryRealSelectionBackupAndInactiveRestore(t *testing.T) {
	for _, state := range []string{"selected", "held", "empty"} {
		t.Run(state, func(t *testing.T) {
			f, _ := selectionInventoryFixture(t, state)
			if err := reviewexec.ValidateFrontSnapshot(t.Context(), f.store.DB()); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_front_frames`).Scan(&n); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			tree := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive-fronts")
			if _, err := Restore(t.Context(), bundle.Path, destination); err != nil {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = reviewexec.ValidateFrontSnapshot(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			after := backupRows(t, db)
			if before["review_front_frames"] != after["review_front_frames"] {
				t.Fatal("restore changed cached source")
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(tree, restoreTree(t, bundle.Path)) {
				t.Fatal("read/restore changed source or bundle")
			}
		})
	}
}
func TestFrontInventoryRejectsResealedCorruptionAndSupportsPredecessor(t *testing.T) {
	for _, query := range []string{
		`UPDATE review_front_frames SET prefix_json='null'`,
		`UPDATE review_front_frames SET prefix_json=replace(replace(prefix_json,',"visible_at_acquisition":true',''),',"visible_at_acquisition":false','')`,
		`UPDATE review_front_frames SET prefix_json=replace(replace(prefix_json,'"visible_at_acquisition":true','"visible_at_acquisition":"yes"'),'"visible_at_acquisition":false','"visible_at_acquisition":"yes"')`,
		`UPDATE review_front_frames SET prefix_json=' []'`,
		`UPDATE review_front_frames SET checked_at=CAST(checked_at AS BLOB)`,
		`UPDATE review_front_frames SET prefix_json=replace(prefix_json,'"number":4','"number":5')`,
		`UPDATE review_front_frames SET source_observed_at='not-a-time'`,
		`UPDATE review_front_frames SET configuration_revision=999`,
		`CREATE INDEX invented_front_history ON review_front_frames(checked_at)`,
		`ALTER TABLE review_front_frames ADD COLUMN invented_credit INTEGER`,
	} {
		t.Run(query[:min(50, len(query))], func(t *testing.T) {
			f, _ := selectionInventoryFixture(t, "held")
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, query)
			assertCurrentSummaryInventoryRefused(t, bundle, false)
		})
	}
	t.Run("missing-optional-domain", func(t *testing.T) {
		f := newRestoreFixture(t)
		if _, err := f.store.DB().Exec(`DROP TABLE review_front_frames;DELETE FROM metadata WHERE key='schema:review_fronts'`); err != nil {
			t.Fatal(err)
		}
		bundle := restoreBundle(t, f)
		if _, err := Inspect(t.Context(), bundle.Path); err != nil {
			t.Fatal("valid predecessor refused", err)
		}
	})
	t.Run("nullable-project-key", func(t *testing.T) {
		f := newRestoreFixture(t)
		bundle := restoreBundle(t, f)
		ddl := strings.Replace(reviewexec.FrontSchemaSQL()[0], "project_id TEXT NOT NULL REFERENCES", "project_id TEXT REFERENCES", 1)
		mutateBundleSQL(t, bundle.Path, `DROP TABLE review_front_frames;`+ddl)
		assertCurrentSummaryInventoryRefused(t, bundle, false)
	})
}
