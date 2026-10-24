package backuphealth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthMirrorAtomicReplacementAndBoundedPrivateReads(t *testing.T) {
	m, _ := healthFixture()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "health.json")
	if err := WriteMirror(path, m); err != nil {
		t.Fatal(err)
	}
	m.Label = "Second backup"
	if err := WriteMirror(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMirror(path)
	if err != nil || got != m {
		t.Fatal(got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary files survived publication", entries, err)
	}
	for _, kind := range []string{"symlink", "directory", "oversize", "unknown-field", "public-permissions"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(dir, kind)
			switch kind {
			case "symlink":
				if err := os.Symlink(path, target); err != nil {
					t.Skip(err)
				}
			case "directory":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(target, []byte(strings.Repeat("x", MaxMirrorBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-field":
				if err := os.WriteFile(target, []byte(`{"private_path":"secret"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "public-permissions":
				if err := os.WriteFile(target, []byte(`{}`), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadMirror(target); err == nil {
				t.Fatal("unsafe report accepted")
			}
			if kind == "symlink" || kind == "directory" || kind == "public-permissions" {
				if err := WriteMirror(target, m); err == nil {
					t.Fatal("unsafe output replaced")
				}
			}
		})
	}
	if _, err := ReadMirror(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	if err := ValidateFiles([]string{path, path}); err == nil {
		t.Fatal("duplicate paths accepted")
	}
	if err := ValidateFiles([]string{"relative"}); err == nil {
		t.Fatal("relative path accepted")
	}
	var files []string
	for i := 0; i < MaxFiles+1; i++ {
		files = append(files, filepath.Join(dir, strings.Repeat("a", i+1)))
	}
	if err := ValidateFiles(files); err == nil {
		t.Fatal("unbounded paths accepted")
	}
}
