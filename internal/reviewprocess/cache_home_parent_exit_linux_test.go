//go:build linux

package reviewprocess

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestCacheHomeLeaseAbruptParent(t *testing.T) {
	mode := os.Getenv("FC_TEST_HOME_LEASE_MODE")
	if mode == "child" {
		if _, err := os.NewFile(3, "inherited HOME lease").Stat(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(os.Getenv("FC_TEST_READY"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(92)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv("FC_TEST_RELEASE")); err == nil {
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(94) // Bounded fail-safe if the parent test never publishes release.
	}
	if mode == "parent" {
		child := exec.Command(os.Args[0], "-test.run=^TestCacheHomeLeaseAbruptParent$")
		child.Env = []string{"FC_TEST_HOME_LEASE_MODE=child", "FC_TEST_READY=" + os.Getenv("FC_TEST_READY"), "FC_TEST_RELEASE=" + os.Getenv("FC_TEST_RELEASE")}
		child.ExtraFiles = []*os.File{os.NewFile(3, "parent HOME lease")}
		if err := child.Start(); err != nil {
			os.Exit(93)
		}
		// No deferred release/child wait: the cooperating child is deliberately alive.
		os.Exit(73)
	}
	home := t.TempDir()
	lease, err := NewCacheHomeLease(home)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	lock, err := lease.acquire(false)
	if err != nil {
		t.Fatal(err)
	}
	ready, release := filepath.Join(home, "ready"), filepath.Join(home, "release")
	// Register before spawning: every later failure must release the child.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	parent := exec.Command(os.Args[0], "-test.run=^TestCacheHomeLeaseAbruptParent$")
	parent.Env = []string{"FC_TEST_HOME_LEASE_MODE=parent", "FC_TEST_READY=" + ready, "FC_TEST_RELEASE=" + release}
	parent.ExtraFiles = []*os.File{lock}
	err = parent.Run()
	lock.Close()
	if parent.ProcessState == nil || parent.ProcessState.ExitCode() != 73 {
		t.Fatal("parent did not exit at boundary", err)
	}
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(ready)
		if e == nil {
			pid, _ = strconv.Atoi(string(b))
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("child readiness absent")
	}
	independent, err := NewCacheHomeLease(home)
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	if acquired, e := independent.acquire(true); e == nil {
		acquired.Close()
		t.Fatal("new instance ignored orphaned child")
	}
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		acquired, e := independent.acquire(true)
		if e == nil {
			acquired.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Preserve failure evidence rather than signalling a possibly reused orphan PID.
	t.Fatal(fmt.Sprintf("inherited lock did not release after child exit: %d", pid))
}
