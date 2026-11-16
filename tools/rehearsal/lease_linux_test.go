//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLeaseSameInodeExclusiveAndReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := acquireLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireLease(path); err == nil {
		second.Close()
		first.Close()
		t.Fatal("accepted concurrent lease")
	}
	first.Close()
	second, err := acquireLease(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}
func TestUnsafeLeaseRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lease")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "relative", dir, alias, filepath.Join(dir, "missing")} {
		if f, err := acquireLease(p); err == nil {
			f.Close()
			t.Fatalf("accepted %q", p)
		}
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if f, err := acquireLease(path); err == nil {
		f.Close()
		t.Fatal("accepted writable lease")
	}
}

func TestExpectedHostLeaseIdentity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lease")
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := acquireLease(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	st := info.Sys().(*syscall.Stat_t)
	if err := requireLeaseIdentity(f, uint64(st.Dev), st.Ino); err != nil {
		t.Fatal(err)
	}
	for _, v := range [][2]uint64{{0, st.Ino}, {uint64(st.Dev), 0}, {uint64(st.Dev), st.Ino + 1}, {uint64(st.Dev) + 1, st.Ino}} {
		if err := requireLeaseIdentity(f, v[0], v[1]); err == nil {
			t.Fatal("accepted wrong host identity")
		}
	}
}
