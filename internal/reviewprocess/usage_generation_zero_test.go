package reviewprocess

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestSupersededZeroUsageCannotReturnExhaustion(t *testing.T) {
	f := newProcessFixture(t)
	first, armed, entered, release, claimed := filepath.Join(f.dir, "first-query"), filepath.Join(f.dir, "zero-captured"), filepath.Join(f.dir, "verify-entered"), filepath.Join(f.dir, "release-verify"), filepath.Join(f.dir, "verify-claimed")
	unblock := func() { _ = os.WriteFile(release, []byte("release"), 0600) }
	t.Cleanup(unblock)
	r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then
 if mkdir %q 2>/dev/null; then
  printf '%%s' %q
  touch %q
 else
  printf '%%s' %q
 fi
else
 printf 'review captured'
fi`, first, usageJSON("owner/repo", "owner", 0, 5, 0, 1), armed, usageJSON("owner/repo", "owner", 3, 5, 0, 1)))
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(f.dir, "blocking-git")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ -e %q ] && mkdir %q 2>/dev/null; then
 touch %q
 while [ ! -e %q ]; do sleep 0.01; done
fi
exec %q "$@"
`, armed, claimed, entered, release, git)
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := reviewworkspace.New(filepath.Join(f.dir, "private-data"), wrapper)
	if err != nil {
		t.Fatal(err)
	}
	r.manager = manager
	type reply struct {
		usage Usage
		err   error
	}
	old := make(chan reply, 1)
	go func() { u, err := r.Usage(t.Context(), f.workspace, "owner/repo", "owner"); old <- reply{u, err} }()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(until) {
			unblock()
			t.Fatal("old zero usage did not reach post-command verification")
		}
		time.Sleep(time.Millisecond)
	}
	newer, err := r.Usage(t.Context(), f.workspace, "owner/repo", "owner")
	if err != nil || newer.Remaining != 3 {
		unblock()
		t.Fatal("newer allowance failed", newer, err)
	}
	unblock()
	select {
	case result := <-old:
		if !errorCode(result.err, "usage_unavailable") || result.usage != (Usage{}) {
			t.Fatal("superseded zero escaped generation fence", result.usage, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded usage did not finish")
	}
	capture, err := r.Review(t.Context(), f.workspace)
	if err != nil || !capture.Started || string(capture.Stdout) != "review captured" {
		t.Fatal("older zero removed newer exact-workspace permit", capture, err)
	}
}
