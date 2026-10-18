package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadNoteBodyFilePreservesPrivateUTF8AndExactBound(t *testing.T) {
	body := strings.Repeat("é", (16<<10)/2)
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok := readNoteBodyFile(path)
	if !ok || got != body {
		t.Fatalf("private UTF-8 body at exact limit: accepted=%v, bytes=%d", ok, len(got))
	}
}

func TestReadNoteBodyFileUsesPlatformPermissionSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte("exact body\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	got, ok := readNoteBodyFile(path)
	if runtime.GOOS == "windows" {
		if !ok || got != "exact body\n" {
			t.Fatalf("Windows regular file refused: accepted=%v, body=%q", ok, got)
		}
	} else if ok || got != "" {
		t.Fatalf("POSIX exposed body accepted: accepted=%v, bytes=%d", ok, len(got))
	}
}

func TestReadNoteBodyFileRejectsInvalidSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "oversized", body: []byte(strings.Repeat("x", (16<<10)+1))},
		{name: "invalid-utf8", body: []byte{0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "body.md")
			if err := os.WriteFile(path, tc.body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, ok := readNoteBodyFile(path); ok {
				t.Fatal("invalid body accepted")
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		if _, ok := readNoteBodyFile(t.TempDir()); ok {
			t.Fatal("directory accepted")
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, ok := readNoteBodyFile(filepath.Join(t.TempDir(), "missing")); ok {
			t.Fatal("missing file accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "body.md")
		if err := os.WriteFile(path, []byte("private body"), 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.md")
		if err := os.Symlink(path, link); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows symlink creation unavailable: %v", err)
			}
			t.Fatal(err)
		}
		if _, ok := readNoteBodyFile(link); ok {
			t.Fatal("symlink accepted")
		}
	})
}
