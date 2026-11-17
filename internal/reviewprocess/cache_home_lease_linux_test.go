//go:build linux

package reviewprocess

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCacheHomeLeaseCrossInstanceAndInheritedChild(t *testing.T) {
	home := t.TempDir()
	first, err := NewCacheHomeLease(home)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewCacheHomeLease(home)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	parent, err := first.acquire(false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "test -e /proc/self/fd/3 || exit 9; echo ready; read line")
	cmd.ExtraFiles = []*os.File{parent}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if text, e := bufio.NewReader(output).ReadString('\n'); e != nil || text != "ready\n" {
		t.Fatal(text, e)
	}
	// Closing every parent lease descriptor simulates loss of supervisor custody;
	// the separately inherited child descriptor still owns the same flock.
	if err = parent.Close(); err != nil {
		t.Fatal(err)
	}
	if lock, e := second.acquire(true); e == nil {
		lock.Close()
		t.Fatal("inherited producer lease lost")
	}
	if _, err = input.Write([]byte("finish\n")); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	lock, err := second.acquire(true)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestCacheHomeLeaseActualRunnerAndReplacement(t *testing.T) {
	home := t.TempDir()
	lease, err := NewCacheHomeLease(home)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	r := &Runner{cacheCustody: &CacheCustody{}, homeLease: lease, env: []string{"HOME=" + home}, executable: "/bin/sh", now: time.Now}
	got := r.run(t.Context(), "/", []string{"-c", "test -e /proc/self/fd/3"}, 1024, 1024)
	if got.err != nil || got.capture.ExitCode != 0 {
		t.Fatal(got)
	}
	if err = r.WithCacheMaintenance(t.Context(), func(observed string) error {
		if observed != home {
			t.Fatal("wrong HOME")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".firecontroller-cache.lock")
	if err = os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = r.WithCacheMaintenance(t.Context(), func(string) error { called = true; return nil }); err == nil || called {
		t.Fatal("replaced lease admitted")
	}
	got = r.run(t.Context(), "/", []string{"-c", "exit 0"}, 1024, 1024)
	if got.err == nil || got.capture.Started {
		t.Fatal("producer accepted replaced lease")
	}
}

func TestCacheHomeLeaseRefusesAliasesAndUnconfiguredRunner(t *testing.T) {
	home := t.TempDir()
	alias := home + "-alias"
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(alias)
	if l, e := NewCacheHomeLease(alias); e == nil {
		l.Close()
		t.Fatal("HOME alias")
	}
	if err := os.WriteFile(filepath.Join(home, ".firecontroller-cache.lock"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if l, e := NewCacheHomeLease(home); e == nil {
		l.Close()
		t.Fatal("public lease")
	}
	r := &Runner{cacheCustody: &CacheCustody{}, env: []string{"HOME=" + home}}
	called := false
	if e := r.WithCacheMaintenance(t.Context(), func(string) error { called = true; return nil }); e == nil || called {
		t.Fatal("unconfigured lease admitted")
	}
}

func TestCacheHomeLeaseReplacementDuringAcquisitionRefuses(t *testing.T) {
	home := t.TempDir()
	lease, err := NewCacheHomeLease(home)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	lock, err := lease.acquireObserved(true, func() {
		path := filepath.Join(home, ".firecontroller-cache.lock")
		if e := os.Rename(path, path+"-old"); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, nil, 0600); e != nil {
			t.Fatal(e)
		}
	})
	if err == nil || lock != nil {
		if lock != nil {
			lock.Close()
		}
		t.Fatal("replaced generation returned")
	}
	// Refusal also releases the abandoned old-inode flock.
	old, err := os.OpenFile(filepath.Join(home, ".firecontroller-cache.lock-old"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err = syscall.Flock(int(old.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal("old lease leaked", err)
	}
}
