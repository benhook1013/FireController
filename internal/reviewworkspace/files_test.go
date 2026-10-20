package reviewworkspace_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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

func TestReviewFileCountRenameThresholdAtOneHundredAndCanonicalPatchUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          int64
	}{
		{"exact", "", 100},
		{"similar_changed_content", "ignored.tmp\n#x\n", 100},
		{"below_threshold", "unrelated replacement content\n", 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			git(t, f.git, f.source, "switch", "candidate")
			if err := os.Rename(filepath.Join(f.source, ".gitignore"), filepath.Join(f.source, "renamed.ignore")); err != nil {
				t.Fatal(err)
			}
			if tc.content != "" {
				write(t, filepath.Join(f.source, "renamed.ignore"), tc.content, 0600)
			}
			for i := 0; i < 97; i++ {
				write(t, filepath.Join(f.source, fmt.Sprintf("added-%03d", i)), fmt.Sprintf("new content %d\n", i), 0600)
			}
			commitChangedFiles(t, &f)
			w := prepare(t, f, "rename_budget")
			raw, err := f.manager.ChangedFiles(t.Context(), w)
			if err != nil || len(raw) != 101 || !slices.Contains(raw, ".gitignore") || !slices.Contains(raw, "renamed.ignore") {
				t.Fatal("complete raw old/new paths changed", len(raw), err)
			}
			count, err := f.manager.ReviewFileCount(t.Context(), w)
			if err != nil || count != tc.want {
				t.Fatal("review budget count changed", count, err)
			}
			// Mutable source refs cannot replace the exact admitted comparison.
			git(t, f.git, f.cache, "update-ref", "refs/heads/main", f.candidate)
			if again, err := f.manager.ReviewFileCount(t.Context(), w); err != nil || again != count {
				t.Fatal("mutable refs changed the review count", again, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, f.git, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--binary", "--full-index", "--no-renames", "--diff-algorithm=myers", "--src-prefix=a/", "--dst-prefix=b/", w.MergeBaseSHA, w.CandidateSHA, "--")
			command.Dir = w.Directory
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "SystemRoot=" + os.Getenv("SystemRoot"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
			patch, err := command.Output()
			hash := sha256.Sum256(patch)
			if err != nil || hex.EncodeToString(hash[:]) != w.PatchSHA256 || f.manager.Verify(t.Context(), w) != nil {
				t.Fatal("review count changed the canonical no-renames patch")
			}
		})
	}
}

func reviewCountShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func reviewCountGit(t *testing.T, f fixture, renamePass string) *reviewworkspace.Manager {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("controlled Git wrapper requires a Unix shell")
	}
	wrapper := filepath.Join(t.TempDir(), "count-git")
	script := "#!/bin/sh\nfor arg do\nif [ \"$arg\" = '--find-renames=50%' ]; then\n" + renamePass + "\nfi\ndone\nexec " + reviewCountShellQuote(f.git) + " \"$@\"\n"
	write(t, wrapper, script, 0700)
	m, err := reviewworkspace.New(f.root, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReviewFileCountValidatesBothSidesBeforeCollapsingExactRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("source name intentionally invalid on Windows")
	}
	f := newFixture(t)
	git(t, f.git, f.source, "switch", "candidate")
	write(t, filepath.Join(f.source, "old\nname"), "exact retained content\n", 0600)
	commitChangedFiles(t, &f)
	base := f.candidate
	if err := os.Rename(filepath.Join(f.source, "old\nname"), filepath.Join(f.source, "portable-name")); err != nil {
		t.Fatal(err)
	}
	commitChangedFiles(t, &f)
	w, err := f.manager.Prepare(t.Context(), 42, "invalid_old_name", f.candidate, base)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "unexpected-exact-pass")
	m := reviewCountGit(t, f, "printf entered > "+reviewCountShellQuote(marker)+"\nexit 0")
	if count, err := m.ReviewFileCount(t.Context(), w); err == nil || count != 0 {
		t.Fatal("an exact rename hid a nonportable original path")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("rename collapsing ran before complete old/new path validation")
	}
}

func TestReviewFileCountRejectsMalformedOrForeignOutputAndFinalWorkspaceChanges(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, output string
		mutate       bool
	}{
		{"unterminated", "source.txt", false},
		{"empty_path", "\x00", false},
		{"nonportable", "bad\\name\x00", false},
		{"foreign", "not-in-raw-list\x00", false},
		{"too_many_logical", "source.txt\x00base-only.txt\x00.gitignore\x00", false},
		{"count_bound", strings.Repeat("source.txt\x00", 10001), false},
		{"output_bound", strings.Repeat("a", (8<<20)+1), false},
		{"final_workspace", "source.txt\x00base-only.txt\x00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := prepare(t, f, "count_"+tc.name)
			output := filepath.Join(t.TempDir(), "paths")
			write(t, output, tc.output, 0600)
			script := ""
			if tc.mutate {
				script = "printf changed > source.txt\n"
			}
			script += "exec /bin/cat " + reviewCountShellQuote(output)
			m := reviewCountGit(t, f, script)
			count, err := m.ReviewFileCount(t.Context(), w)
			if err == nil || count != 0 || strings.Contains(err.Error(), f.root) || strings.Contains(err.Error(), tc.output) {
				t.Fatal("invalid comparison count accepted or disclosed private details")
			}
		})
	}
}

func TestReviewFileCountEmptyAndCancelledReads(t *testing.T) {
	f := newFixture(t)
	w, err := f.manager.Prepare(t.Context(), 42, "empty_count", f.candidate, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := f.manager.ReviewFileCount(t.Context(), w); err != nil || count != 0 {
		t.Fatal("empty comparison did not return an explicit zero count", count, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if count, err := f.manager.ReviewFileCount(ctx, w); err == nil || count != 0 {
		t.Fatal("cancelled comparison accepted")
	}
	if _, err := f.manager.ReviewFileCount(nil, w); err == nil {
		t.Fatal("nil context accepted")
	}
	var missing *reviewworkspace.Manager
	if _, err := missing.ReviewFileCount(t.Context(), w); err == nil {
		t.Fatal("nil manager accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	marker := filepath.Join(t.TempDir(), "entered")
	m := reviewCountGit(t, f, "printf entered > "+reviewCountShellQuote(marker)+"\nexec /bin/sleep 5")
	ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.ReviewFileCount(ctx, w); done <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, entered := os.Stat(marker)
	cancel()
	select {
	case err := <-done:
		if entered != nil || err == nil || strings.Contains(err.Error(), f.root) {
			t.Fatal("rename-count pass did not cancel safely")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled rename-count pass did not join")
	}
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
