//go:build unix

package reviewprocess

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestCacheCustodyActualChildrenAndCancellation(t *testing.T) {
	c := &CacheCustody{}
	r := &Runner{cacheCustody: c, executable: "/bin/sh", now: time.Now}
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan runResult, 1)
	go func() {
		done <- r.run(ctx, "/", []string{"-c", "while :; do :; done"}, 1024, 1024, func(time.Time) { close(entered); <-release })
	}()
	<-entered
	cancel()
	called := false
	if err := c.TryMaintenance(t.Context(), func() error { called = true; return nil }); err == nil || called {
		t.Fatal("cancellation released active child custody")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child not reaped")
	}
	if err := c.TryMaintenance(t.Context(), func() error { called = true; return nil }); err != nil || !called {
		t.Fatal(err)
	}
}

func TestCacheCustodySharedRunnersRespectDeadlineDuringMaintenance(t *testing.T) {
	c := &CacheCustody{}
	for _, r := range []*Runner{{executable: "/bin/sh", now: time.Now}, {executable: "/bin/sh", now: time.Now}} {
		if err := WithCacheCustody(c)(r); err != nil {
			t.Fatal(err)
		}
		if err := c.TryMaintenance(t.Context(), func() error {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			got := r.run(ctx, "/", []string{"-c", "exit 0"}, 1024, 1024)
			if !errors.Is(got.err, context.DeadlineExceeded) || got.capture.Started {
				t.Fatal("producer started during maintenance")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		got := r.run(t.Context(), "/", []string{"-c", "exit 0"}, 1024, 1024)
		if got.err != nil || !got.capture.Started {
			t.Fatal(got)
		}
	}
	sentinel := errors.New("callback failure")
	if err := c.TryMaintenance(t.Context(), func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := c.TryMaintenance(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCacheCustodyUnresolvedOwnedGroupFences(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "while :; do :; done")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL); _ = command.Wait() }()
	c := &CacheCustody{}
	c.track(command.Process.Pid)
	called := false
	if err := c.TryMaintenance(t.Context(), func() error { called = true; return nil }); err == nil || called {
		t.Fatal("live group accepted")
	}
	c.groupsMu.Lock()
	c.unresolved = true
	c.groupsMu.Unlock()
	if err := c.TryMaintenance(t.Context(), func() error { called = true; return nil }); err == nil || called {
		t.Fatal("overflow accepted")
	}
}
