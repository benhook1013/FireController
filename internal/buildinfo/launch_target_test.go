package buildinfo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestLaunchTargetReadsOnlyBoundedExecutableProvenance(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fcmcp")
	if err := os.WriteFile(path, []byte("synthetic metadata fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	metadata := &debug.BuildInfo{Path: "github.com/benhook1013/FireController/cmd/fcmcp", Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: commit}, {Key: "vcs.modified", Value: "false"}}}
	reads := 0
	read := func(got string) (*debug.BuildInfo, error) {
		reads++
		if got != path {
			t.Fatal("selected another launch path")
		}
		return metadata, nil
	}
	if got, ok := launchTarget(path, read); !ok || got != (Source{Revision: commit, State: "clean"}) {
		t.Fatal("known target lost", got, ok)
	}
	for _, bad := range []string{"fcmcp", filepath.Join(root, "missing", "fcmcp"), root, filepath.Join(root, "other-program")} {
		before := reads
		if _, ok := launchTarget(bad, read); ok || reads != before {
			t.Fatal("unsafe path reached metadata reader", bad)
		}
	}
	directory := filepath.Join(root, "directory", "fcmcp")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, ok := launchTarget(directory, read); ok {
		t.Fatal("directory became executable provenance")
	}
	for _, mutate := range []func(){func() { metadata.Path = "unrelated/cmd/fcmcp" }, func() { metadata.Settings = nil }, func() {
		metadata.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: "PRIVATE_INVALID_VALUE"}}
	}} {
		mutate()
		if _, ok := launchTarget(path, read); ok {
			t.Fatal("invented provenance from unrelated or unknown target")
		}
	}
	if _, ok := launchTarget(path, func(string) (*debug.BuildInfo, error) { return nil, errors.New("PRIVATE_PATH_ERROR") }); ok {
		t.Fatal("failed metadata read became available")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		before := reads
		if _, ok := launchTarget(path, read); ok || reads != before {
			t.Fatal("nonexecutable file reached metadata reader")
		}
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate((256 << 20) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	before := reads
	if _, ok := launchTarget(path, read); ok || reads != before {
		t.Fatal("oversized executable reached metadata reader")
	}
}

func TestLaunchTargetPreservesStableDirectoryLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory link permission varies on Windows")
	}
	root := t.TempDir()
	for _, rev := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(root, rev, "bin"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rev, "bin", "fc"), []byte(rev), 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "current")
	if err := os.Symlink("one", link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "bin", "fc")
	read := func(selected string) (*debug.BuildInfo, error) {
		if selected != path {
			t.Fatal("resolved away the stable launch path")
		}
		body, err := os.ReadFile(selected)
		if err != nil {
			return nil, err
		}
		rev := strings.Repeat("a", 40)
		if string(body) == "two" {
			rev = strings.Repeat("b", 40)
		}
		return &debug.BuildInfo{Path: "github.com/benhook1013/FireController/cmd/fc", Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}}, nil
	}
	first, ok := launchTarget(path, read)
	if !ok {
		t.Fatal("initial target unavailable")
	}
	replacement := filepath.Join(root, "next")
	if err := os.Symlink("two", replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, link); err != nil {
		t.Fatal(err)
	}
	second, ok := launchTarget(path, read)
	if !ok || first.Revision == second.Revision {
		t.Fatal("stable path did not follow atomic installation selection", first, second)
	}
	finalLink := filepath.Join(root, "fc")
	if err := os.Symlink(filepath.Join("two", "bin", "fc"), finalLink); err != nil {
		t.Fatal(err)
	}
	got, ok := launchTarget(finalLink, func(selected string) (*debug.BuildInfo, error) {
		if selected != finalLink {
			t.Fatal("resolved away stable binary link")
		}
		return &debug.BuildInfo{Path: "github.com/benhook1013/FireController/cmd/fc", Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: second.Revision}, {Key: "vcs.modified", Value: "false"}}}, nil
	})
	if !ok || got != second {
		t.Fatal("stable final binary link unavailable", got, ok)
	}
}
