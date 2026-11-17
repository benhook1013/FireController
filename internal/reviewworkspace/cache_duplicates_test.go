package reviewworkspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func duplicateFixture(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	root := filepath.Join(t.TempDir(), "owned")
	m, err := New(root, git)
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := m.CachePath(77)
	fetchFixtureGit(t, root, "init", "--bare", "--template=", cache)
	if err = os.Chmod(cache, 0700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(t.TempDir(), "blob")
	if err = os.WriteFile(blob, []byte("retained bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := fetchFixtureGit(t, root, "--git-dir="+cache, "hash-object", "-w", blob)
	fetchFixtureGit(t, root, "--git-dir="+cache, "update-ref", "refs/fixture/keep", id)
	// Without -d, repack leaves the original loose copy in place.
	fetchFixtureGit(t, root, "--git-dir="+cache, "repack", "-a")
	if _, err = os.Stat(filepath.Join(cache, "objects", id[:2], id[2:])); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(blob, []byte("unique unreachable bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unique := fetchFixtureGit(t, root, "--git-dir="+cache, "hash-object", "-w", blob)
	return m, cache, id, unique
}
func TestCacheDuplicatesPreserveSharedAndUniqueObjects(t *testing.T) {
	m, cache, id, unique := duplicateFixture(t)
	shared := filepath.Join(t.TempDir(), "shared")
	fetchFixtureGit(t, m.root, "clone", "--shared", "--no-checkout", cache, shared)
	for pass := 0; pass < 2; pass++ {
		if err := m.PrunePackedDuplicates(t.Context(), 77); err != nil {
			t.Fatal(err)
		}
		if got := fetchFixtureGit(t, shared, "cat-file", "-p", id); got != "retained bytes" {
			t.Fatal(got)
		}
		if got := fetchFixtureGit(t, shared, "cat-file", "-p", unique); got != "unique unreachable bytes" {
			t.Fatal(got)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "objects", id[:2], id[2:])); !os.IsNotExist(err) {
		t.Fatal("duplicate not removed", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "objects", unique[:2], unique[2:])); err != nil {
		t.Fatal("unique removed", err)
	}
}
func TestCacheDuplicatesRefuseCorruptPack(t *testing.T) {
	m, cache, id, _ := duplicateFixture(t)
	packs, _ := filepath.Glob(filepath.Join(cache, "objects", "pack", "*.pack"))
	data, err := os.ReadFile(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err = os.Chmod(packs[0], 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(packs[0], data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.PrunePackedDuplicates(t.Context(), 77); err == nil {
		t.Fatal("corrupt replacement accepted")
	}
	if _, err = os.Stat(filepath.Join(cache, "objects", id[:2], id[2:])); err != nil {
		t.Fatal("good loose copy lost", err)
	}
}
func TestCacheDuplicatesRefuseSymlinkAndHonorFetchGate(t *testing.T) {
	m, cache, id, _ := duplicateFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cache, "objects", "ab")
	if _, err := os.Lstat(link); os.IsNotExist(err) {
		if err = os.Symlink(filepath.Dir(outside), link); err != nil {
			t.Fatal(err)
		}
		if err = m.PrunePackedDuplicates(t.Context(), 77); err == nil {
			t.Fatal("symlink accepted")
		}
		if err = os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	m.fetchGate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := m.PrunePackedDuplicates(ctx, 77); err == nil {
		t.Fatal("ignored writer gate")
	}
	<-m.fetchGate
	if _, err := os.Stat(filepath.Join(cache, "objects", id[:2], id[2:])); err != nil {
		t.Fatal("changed while excluded")
	}
	if data, _ := os.ReadFile(outside); strings.TrimSpace(string(data)) != "outside" {
		t.Fatal("outside touched")
	}
}

func TestCacheDuplicatesRejectAlternatesAndBoundDirectory(t *testing.T) {
	m, cache, id, _ := duplicateFixture(t)
	if err := os.WriteFile(filepath.Join(cache, "objects", "info", "alternates"), []byte("/not-owned/objects\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.PrunePackedDuplicates(t.Context(), 77); err == nil {
		t.Fatal("external pack authority accepted")
	}
	if _, err := os.Stat(filepath.Join(cache, "objects", id[:2], id[2:])); err != nil {
		t.Fatal("loose copy changed")
	}
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readDuplicateDirectory(dir, 1); err == nil {
		t.Fatal("directory overflow accepted")
	}
	if entries, err := readDuplicateDirectory(dir, 2); err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
}
