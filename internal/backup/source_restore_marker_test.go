package backup

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Interrupted Restore can leave a copied predecessor database with a normal or
// previously active identity. That database must not escape its filesystem hold
// through operator backup creation, even though its contents otherwise validate.
func TestCreateFromDirectoryRefusesRestoreMarkerBeforeOpeningSource(t *testing.T) {
	for _, kind := range []string{"ordinary", "coordination-active"} {
		t.Run(kind, func(t *testing.T) {
			var dir string
			if kind == "ordinary" {
				f := newRestoreFixture(t)
				if err := f.store.Close(); err != nil {
					t.Fatal(err)
				}
				dir = f.dir
			} else {
				f := activationTestFixture(t)
				if _, err := Activate(t.Context(), f.dir, f.request); err != nil {
					t.Fatal(err)
				}
				dir = f.dir
			}
			// Establish genuine source validity and actual backup eligibility before
			// adding only the interrupted-restore marker; no recovery metadata is forged.
			if err := ValidateSource(t.Context(), dir); err != nil {
				t.Fatal("unmarked active source invalid", err)
			}
			unmarked, err := CreateFromDirectory(t.Context(), dir, filepath.Join(t.TempDir(), "control-backups"))
			if err != nil || unmarked.State != "complete" {
				t.Fatal("unmarked active source cannot publish", err)
			}
			marker := filepath.Join(dir, restoreMarker)
			if err := os.WriteFile(marker, []byte("controlled interrupted restore\n"), 0600); err != nil {
				t.Fatal(err)
			}
			before := restoreTree(t, dir)
			destination := filepath.Join(t.TempDir(), "must-not-exist")
			_, err = CreateFromDirectory(t.Context(), dir, destination)
			if err == nil {
				t.Fatal("marked source published a backup despite incomplete restore")
			}
			backupCode(t, err, "restore_inactive")
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatal("marked source created a backup destination")
			}
			if after := restoreTree(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed marked source bytes or files")
			}
			backupCode(t, ValidateSource(t.Context(), dir), "restore_inactive")
			if after := restoreTree(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("preflight changed marked source bytes or files")
			}
		})
	}
}
