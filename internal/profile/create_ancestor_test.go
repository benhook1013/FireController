package profile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCreateAllowsHigherDirectorySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink fixture")
	}
	root := setupTestDirectory(t)
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "ancestor")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(alias, "private", "profiles.json")
	if _, err := Create(path, setupTestBinding()); err != nil {
		t.Fatal("higher system-style symlink rejected", err)
	}
	if _, err := Create(path, setupTestBinding()); err != nil {
		t.Fatal("exact retry through ancestor failed", err)
	}
	// Immediate parents still must be real private directories.
	if _, err := Create(filepath.Join(alias, "other.json"), setupTestBinding()); err == nil {
		t.Fatal("symlinked immediate parent accepted")
	}
}
