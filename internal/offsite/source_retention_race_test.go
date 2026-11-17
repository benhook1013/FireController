package offsite

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOffsiteSourceSelectionSkipsOnlyRemovedEnumeratedBundle(t *testing.T) {
	f := newFixture(t)
	old := f.bundle(t)
	latest := f.bundle(t)
	oldManifest, err := os.ReadFile(filepath.Join(old.Path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Capture the actual enumeration, then deterministically perform the local
	// retention deletion before selection reads that entry's manifest.
	entries, err := os.ReadDir(f.c.SourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(old.Path); err != nil {
		t.Fatal(err)
	}
	selected, err := newestEntries(t.Context(), f.c, entries)
	if err != nil || selected.ID != latest.ID || selected.State != "verified" {
		t.Fatal("removed old bundle blocked the surviving verified source", selected, err)
	}
	for _, kind := range []string{"missing_manifest", "malformed_manifest", "symlink", "private_permissions"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "private_permissions" && runtime.GOOS == "windows" {
				t.Skip("POSIX permission boundary")
			}
			if kind == "symlink" {
				if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), old.Path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(old.Path, 0700); err != nil {
					t.Fatal(err)
				}
				if kind != "missing_manifest" {
					mode := os.FileMode(0600)
					body := []byte(`{"id":"not-a-manifest"}`)
					if kind == "private_permissions" {
						mode = 0644
						body = oldManifest
					}
					if err := os.WriteFile(filepath.Join(old.Path, "manifest.json"), body, mode); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(old.Path); err != nil {
					t.Error(err)
				}
			})
			_, err := newestEntries(t.Context(), f.c, entries)
			wantCode(t, err, "offsite_source_invalid")
		})
	}
}
