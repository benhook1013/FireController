//go:build offsite_integration

package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholds"
)

// Explicit opt-in qualification, never a live restore. Linux namespaces remove
// all network interfaces except a down loopback device; PATH is empty, and HOME
// contains no credentials. The synthetic predecessor retains its old schema.
func TestPackagedOfflineCurrentAndPredecessorRestore(t *testing.T) {
	operator := os.Getenv("FC_TEST_FCD")
	if operator == "" {
		t.Skip("set FC_TEST_FCD to the packaged test executable")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux namespace qualification")
	}
	if !filepath.IsAbs(operator) {
		t.Fatal("explicit absolute packaged executable required")
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Fatal("offline qualification requires unshare", err)
	}
	for _, predecessor := range []bool{false, true} {
		name := "current"
		if predecessor {
			name = "adoption-v1"
		}
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t)
			if predecessor {
				for _, table := range []string{"review_adoption_retargets", "review_adoption_resolutions", "review_adoption_targets", "review_adoption_obligations", "review_adoption_imports", "review_adoption_sources"} {
					if _, err := f.store.DB().Exec("DROP TABLE " + table); err != nil {
						t.Fatal(err)
					}
				}
				for _, statement := range adoptionholds.SchemaV1SQL() {
					if _, err := f.store.DB().Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.store.DB().Exec("UPDATE metadata SET value='1' WHERE key='schema:review_adoption_obligations'"); err != nil {
					t.Fatal(err)
				}
			}
			bundle := restoreBundle(t, f)
			sealed := restoreTree(t, bundle.Path)
			var originalVersion string
			if err := f.store.DB().QueryRow("SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'").Scan(&originalVersion); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			cmd := exec.CommandContext(t.Context(), unshare, "--user", "--map-root-user", "--net", operator, "restore", "--bundle", bundle.Path, "--data-dir", destination)
			cmd.Env = []string{"PATH=", "HOME=" + t.TempDir()}
			var diagnostic bytes.Buffer
			cmd.Stderr = &diagnostic
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("network-isolated packaged restore: %v %s", err, diagnostic.String())
			}
			var restored RestoreSummary
			if err := json.Unmarshal(output, &restored); err != nil || restored.State != "inactive" {
				t.Fatal("inactive packaged restore result", err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var version string
			if err := db.QueryRow("SELECT value FROM metadata WHERE key='schema:review_adoption_obligations'").Scan(&version); err != nil || version != originalVersion {
				t.Fatal("restore migrated schema", version, err)
			}
			state, err := readRecoveryStatus(t.Context(), db)
			if err != nil || !state.ProvidersHeld || state.State != "inactive" {
				t.Fatal("provider recovery fence", state, err)
			}
			if !reflect.DeepEqual(sealed, restoreTree(t, bundle.Path)) {
				t.Fatal("restore modified sealed input")
			}
		})
	}
}
