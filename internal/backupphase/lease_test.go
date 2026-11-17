package backupphase

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func leaseFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "phase.lock")
	if e := os.WriteFile(p, nil, 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestLeaseCancellationAndRelease(t *testing.T) {
	p := leaseFile(t)
	release, e := Acquire(t.Context(), p)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if r, e := Acquire(ctx, p); e == nil {
		r()
		t.Fatal("overlapping owner")
	} else if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	release()
	r, e := Acquire(t.Context(), p)
	if e != nil {
		t.Fatal(e)
	}
	r()
	if _, e := os.Stat(p); e != nil {
		t.Fatal("release removed shared inode", e)
	}
}
func TestLeaseRefusesUnprovisionedAndSymlink(t *testing.T) {
	p := leaseFile(t)
	for _, bad := range []string{"relative", p + "missing", filepath.Dir(p)} {
		if r, e := Acquire(t.Context(), bad); e == nil {
			r()
			t.Fatal(bad)
		}
	}
	link := p + "link"
	if os.Symlink(p, link) == nil {
		if r, e := Acquire(t.Context(), link); e == nil {
			r()
			t.Fatal("symlink")
		}
	}
}
func TestLeaseProcessDeath(t *testing.T) {
	if p := os.Getenv("FC_PHASE_TEST_CHILD"); p != "" {
		r, e := Acquire(context.Background(), p)
		if e != nil {
			os.Exit(2)
		}
		defer r()
		os.Stdout.Write([]byte("ready\n"))
		time.Sleep(time.Minute)
		return
	}
	p := leaseFile(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseProcessDeath$")
	cmd.Env = append(os.Environ(), "FC_PHASE_TEST_CHILD="+p)
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	buf := make([]byte, 6)
	if _, e = io.ReadFull(out, buf); e != nil || string(buf) != "ready\n" {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if r, e := Acquire(ctx, p); e == nil {
		r()
		t.Fatal("child did not own lock")
	} else if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	cmd.Wait()
	ctx2, cancel2 := context.WithTimeout(t.Context(), time.Second)
	defer cancel2()
	r, e := Acquire(ctx2, p)
	if e != nil {
		t.Fatal("crash did not release", e)
	}
	r()
}

func TestLeaseRefusesReplaceableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory custody")
	}
	dir := filepath.Join(t.TempDir(), "unsafe")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "phase.lock")
	if e := os.WriteFile(p, nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(dir, 0777); e != nil {
		t.Fatal(e)
	}
	if r, e := Acquire(t.Context(), p); e == nil {
		r()
		t.Fatal("replaceable inode admitted")
	}
}
