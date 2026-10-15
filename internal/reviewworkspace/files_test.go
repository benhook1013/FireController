package reviewworkspace_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func commitChangedFiles(t *testing.T, f *fixture) {
	t.Helper()
	git(t, f.git, f.source, "add", "--all")
	git(t, f.git, f.source, "commit", "-m", "synthetic changed paths")
	f.candidate = git(t, f.git, f.source, "rev-parse", "HEAD")
	git(t, f.git, f.cache, "fetch", "--no-tags", "--", f.source, "refs/heads/candidate:refs/heads/candidate")
}

func TestChangedFilesExactComparisonSortedAndPinned(t *testing.T) {
	f := newFixture(t)
	git(t, f.git, f.source, "switch", "candidate")
	if err := os.Remove(filepath.Join(f.source, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.source, "source.txt"), filepath.Join(f.source, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.source, "nested space"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"z.txt", "--leading-option", "nested space/naïve 日本語.txt", "quote\"name.txt"} {
		write(t, filepath.Join(f.source, filepath.FromSlash(path)), "synthetic\n", 0600)
	}
	commitChangedFiles(t, &f)
	w := prepare(t, f, "changed_paths")
	w, err := f.manager.BindRepository(context.Background(), w, "synthetic/changed-files")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "base-only.txt", "source.txt", "renamed.txt", "z.txt", "--leading-option", "nested space/naïve 日本語.txt", "quote\"name.txt"}
	sort.Strings(want)
	got, err := f.manager.ChangedFiles(context.Background(), w)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("complete source comparison: %q, %v; wanted %q", got, err, want)
	}
	// A source rename appears as a delete and add. The comparison starts at the
	// merge base, so base-only.txt remains present even though it is also on base.
	write(t, filepath.Join(f.source, "later.txt"), "later branch contents\n", 0600)
	commitChangedFiles(t, &f)
	git(t, f.git, f.cache, "update-ref", "refs/heads/main", f.candidate)
	again, err := f.manager.ChangedFiles(context.Background(), w)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatal("mutable branch refs changed the admitted path set")
	}
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal("path read changed the workspace")
	}
}

func TestChangedFilesEmptyIsSuccessfulAndCancellationIsSafe(t *testing.T) {
	f := newFixture(t)
	w, err := f.manager.Prepare(context.Background(), 42, "empty_paths", f.candidate, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := f.manager.ChangedFiles(context.Background(), w)
	if err != nil || paths == nil || len(paths) != 0 {
		t.Fatalf("empty comparison must be an explicit empty result: %v %v", paths, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	paths, err = f.manager.ChangedFiles(ctx, w)
	if err == nil || paths != nil || time.Since(started) > time.Second || strings.Contains(err.Error(), f.root) {
		t.Fatal("cancelled read did not fail promptly and safely")
	}
	if _, err := f.manager.ChangedFiles(nil, w); err == nil {
		t.Fatal("nil context accepted")
	}
	var missing *reviewworkspace.Manager
	if _, err := missing.ChangedFiles(context.Background(), w); err == nil {
		t.Fatal("missing manager accepted")
	}
}

func TestChangedFilesRejectsTamperedWorkspace(t *testing.T) {
	f := newFixture(t)
	for name, change := range map[string]func(*reviewworkspace.Workspace){
		"tracked": func(w *reviewworkspace.Workspace) {
			write(t, filepath.Join(w.Directory, "source.txt"), "changed", 0600)
		},
		"untracked":  func(w *reviewworkspace.Workspace) { write(t, filepath.Join(w.Directory, "extra.txt"), "extra", 0600) },
		"candidate":  func(w *reviewworkspace.Workspace) { w.CandidateSHA = f.base },
		"merge_base": func(w *reviewworkspace.Workspace) { w.MergeBaseSHA = f.base },
		"patch":      func(w *reviewworkspace.Workspace) { w.PatchSHA256 = strings.Repeat("a", 64) },
		"repository": func(w *reviewworkspace.Workspace) { w.RepositoryCanonical = "other/repository" },
		"comparison": func(w *reviewworkspace.Workspace) {
			git(t, f.git, w.Directory, "update-ref", w.ComparisonRef, f.candidate)
		},
		"directory": func(w *reviewworkspace.Workspace) { w.Directory = f.source },
	} {
		t.Run(name, func(t *testing.T) {
			w := prepare(t, f, "paths_"+name)
			change(&w)
			paths, err := f.manager.ChangedFiles(context.Background(), w)
			if err == nil || paths != nil || strings.Contains(err.Error(), f.root) {
				t.Fatal("tampered identity accepted or exposed private paths")
			}
		})
	}
}

func TestChangedFilesRejectsNonportableSourceNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("names intentionally invalid on Windows")
	}
	for _, name := range []string{"bad\nname", "bad\tname", "bad\\name", "C:drive-relative", "invalid-\xff"} {
		t.Run(fmt.Sprintf("%x", name), func(t *testing.T) {
			f := newFixture(t)
			git(t, f.git, f.source, "switch", "candidate")
			write(t, filepath.Join(f.source, name), "synthetic\n", 0600)
			commitChangedFiles(t, &f)
			w := prepare(t, f, "nonportable")
			paths, err := f.manager.ChangedFiles(context.Background(), w)
			if err == nil || paths != nil || strings.Contains(err.Error(), name) || strings.Contains(err.Error(), f.root) {
				t.Fatal("nonportable path was accepted, truncated or disclosed")
			}
		})
	}
}

func TestChangedFilesCountBoundReturnsNoPartialList(t *testing.T) {
	f := newFixture(t)
	git(t, f.git, f.source, "switch", "candidate")
	if err := os.Mkdir(filepath.Join(f.source, "many"), 0700); err != nil {
		t.Fatal(err)
	}
	// Two existing changes plus these 9,999 paths exceed the 10,000-entry bound.
	// This uses real Git trees and one bounded fixture, rather than fake output.
	for i := 0; i < 9999; i++ {
		write(t, filepath.Join(f.source, "many", fmt.Sprintf("file-%05d", i)), "", 0600)
	}
	commitChangedFiles(t, &f)
	w := prepare(t, f, "too_many_paths")
	paths, err := f.manager.ChangedFiles(context.Background(), w)
	if err == nil || paths != nil || strings.Contains(err.Error(), f.root) {
		t.Fatal("oversized source comparison returned a partial or successful list")
	}
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal("bounded read mutated the workspace")
	}
}
