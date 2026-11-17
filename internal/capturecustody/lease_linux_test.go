//go:build linux

package capturecustody

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func deadline(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}
func root(t *testing.T) string {
	t.Helper()
	p := t.TempDir()
	if err := os.Chmod(p, 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConcurrentWriterAndExclusiveCollector(t *testing.T) {
	p := root(t)
	ctx := deadline(t)
	a, err := Writer(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Writer(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err = Collector(ctx, p); !errors.Is(err, ErrBusy) {
		t.Fatalf("collector=%v", err)
	}
	a.Close()
	if _, err = Collector(ctx, p); !errors.Is(err, ErrBusy) {
		t.Fatalf("second writer not protected: %v", err)
	}
	b.Close()
	c, err := Collector(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err = Writer(short, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer=%v", err)
	}
}

func TestLeasePathReplacementRefuses(t *testing.T) {
	p := root(t)
	l, err := Writer(deadline(t), p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	path := filepath.Join(p, lockName)
	if err = os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(l.Validate(), ErrUnavailable) {
		t.Fatal("replacement accepted")
	}
}

func TestUnsafeRootsLocksAndUnboundedWaitRefuse(t *testing.T) {
	p := root(t)
	if _, err := Writer(context.Background(), p); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	link := filepath.Join(root(t), "alias")
	os.Symlink(p, link)
	if _, err := Writer(deadline(t), link); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	path := filepath.Join(p, lockName)
	os.Symlink(filepath.Join(p, "missing"), path)
	if _, err := Writer(deadline(t), p); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	os.Remove(path)
	os.WriteFile(path, nil, 0644)
	if _, err := Writer(deadline(t), p); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

// The helper holds publication custody even after closing its staging FD.
// This models exactly the dangerous gap in all three existing stores.
func TestPublicationProcessHelper(t *testing.T) {
	if os.Getenv("CAPTURE_CUSTODY_HELPER") != "1" {
		return
	}
	p := os.Getenv("CAPTURE_CUSTODY_ROOT")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := Writer(ctx, p)
	if err != nil {
		os.Exit(10)
	}
	defer l.Close()
	stage := filepath.Join(p, ".capture-stage-v1-test")
	if os.WriteFile(stage, []byte("retained capture"), 0600) != nil {
		os.Exit(11)
	}
	if os.Getenv("CAPTURE_CUSTODY_LINK") == "1" {
		if os.Link(stage, filepath.Join(p, "attempt.json")) != nil {
			os.Exit(12)
		}
	}
	// Tell parent that file descriptor is closed but writer custody remains.
	os.Stdout.Write([]byte("ready\n"))
	var b [1]byte
	os.Stdin.Read(b[:])
}

func TestProcessCustodyAndCrashRelease(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_publication", true: "after_publication"}[linked], func(t *testing.T) {
			p := root(t)
			ctx := deadline(t)
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPublicationProcessHelper$")
			cmd.Env = append(os.Environ(), "CAPTURE_CUSTODY_HELPER=1", "CAPTURE_CUSTODY_ROOT="+p)
			if linked {
				cmd.Env = append(cmd.Env, "CAPTURE_CUSTODY_LINK=1")
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			b := make([]byte, 6)
			if n, err := io.ReadFull(out, b); err != nil || n != 6 || string(b) != "ready\n" {
				t.Fatalf("ready=%q/%v", b, err)
			}
			if _, err = Collector(ctx, p); !errors.Is(err, ErrBusy) {
				t.Fatalf("active publication gap unprotected: %v", err)
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()
			c, err := Collector(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			stage := filepath.Join(p, ".capture-stage-v1-test")
			if data, err := os.ReadFile(stage); err != nil || string(data) != "retained capture" {
				t.Fatal("custody must not delete sole copy")
			}
			if linked {
				a, _ := os.Stat(stage)
				b, err := os.Stat(filepath.Join(p, "attempt.json"))
				if err != nil || !os.SameFile(a, b) {
					t.Fatal("published link changed")
				}
			}
		})
	}
}
