package reviewworkspace_test

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func openPrepared(m *reviewworkspace.Manager, ctx context.Context, w reviewworkspace.Workspace) (reviewworkspace.Workspace, error) {
	return m.Open(ctx, filepath.Base(w.Directory), w.CandidateSHA, w.BaseSHA, w.MergeBaseSHA, w.PatchSHA256, w.RepositoryCanonical)
}

func boundOpenFixture(t *testing.T, f fixture, id string) reviewworkspace.Workspace {
	t.Helper()
	w, err := f.manager.BindRepository(context.Background(), prepare(t, f, id), "synthetic/open-proof")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

type openFileState struct {
	Mode     os.FileMode
	Size     int64
	Modified time.Time
	SHA256   [32]byte
	Link     string
}

func openFilesystemState(t *testing.T, root string) map[string]openFileState {
	t.Helper()
	state := make(map[string]openFileState)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := openFileState{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.SHA256 = sha256.Sum256(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
			value.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[relative] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestOpenReconstructsBoundWorkspaceAfterManagerRecreation(t *testing.T) {
	f := newFixture(t)
	w := boundOpenFixture(t, f, "reopen")
	before := openFilesystemState(t, f.root)
	got, err := openPrepared(f.manager, context.Background(), w)
	if err != nil || got != w {
		t.Fatalf("reconstructed workspace differs: %+v %v", got, err)
	}
	// Manager owns no Close resource or per-attempt map. A newly constructed
	// manager must reopen existing source without preparation or redispatch.
	fresh, err := reviewworkspace.New(f.root, f.git)
	if err != nil {
		t.Fatal(err)
	}
	got, err = openPrepared(fresh, context.Background(), w)
	if err != nil || got != w {
		t.Fatalf("fresh manager cannot reconstruct admitted source: %v", err)
	}
	if !reflect.DeepEqual(before, openFilesystemState(t, f.root)) {
		t.Fatal("reopening changed owned filesystem state")
	}
}

func TestOpenDoesNotRefreshIndexAfterWorktreeStatChange(t *testing.T) {
	f := newFixture(t)
	w := boundOpenFixture(t, f, "stat_only")
	path := filepath.Join(w.Directory, "source.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Contents remain identical, but the cached Git index stat is now stale.
	// Ordinary git status may rewrite that index unless optional locks are off.
	changed := info.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	before := openFilesystemState(t, f.root)
	got, err := openPrepared(f.manager, context.Background(), w)
	if err != nil || got != w {
		t.Fatalf("unchanged source content was refused: %v", err)
	}
	if !reflect.DeepEqual(before, openFilesystemState(t, f.root)) {
		t.Fatal("source reopening refreshed the index or otherwise changed owned state")
	}
}

func TestOpenInvalidAndMissingIdentitiesHaveNoFilesystemEffects(t *testing.T) {
	f := newFixture(t)
	w := boundOpenFixture(t, f, "valid")
	before := openFilesystemState(t, f.root)
	for _, id := range []string{"missing", "", "..", "../valid", f.source, "a/b", "a\\b", "é", strings.Repeat("a", 129)} {
		got, err := f.manager.Open(context.Background(), id, w.CandidateSHA, w.BaseSHA, w.MergeBaseSHA, w.PatchSHA256, w.RepositoryCanonical)
		if err == nil || got != (reviewworkspace.Workspace{}) || strings.Contains(err.Error(), f.root) {
			t.Fatal("missing or arbitrary path identity accepted or disclosed")
		}
	}
	for _, change := range []func(*reviewworkspace.Workspace){
		func(w *reviewworkspace.Workspace) { w.CandidateSHA = "HEAD" },
		func(w *reviewworkspace.Workspace) { w.BaseSHA = strings.ToUpper(f.base) },
		func(w *reviewworkspace.Workspace) { w.MergeBaseSHA = w.MergeBaseSHA[:39] },
		func(w *reviewworkspace.Workspace) { w.PatchSHA256 = strings.Repeat("z", 64) },
		func(w *reviewworkspace.Workspace) { w.RepositoryCanonical = "" },
		func(w *reviewworkspace.Workspace) { w.RepositoryCanonical = "owner/../repository" },
		func(w *reviewworkspace.Workspace) { w.RepositoryCanonical = "https://github.com/owner/repository" },
	} {
		invalid := w
		change(&invalid)
		got, err := openPrepared(f.manager, context.Background(), invalid)
		if err == nil || got != (reviewworkspace.Workspace{}) {
			t.Fatal("malformed journal identity accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	got, err := openPrepared(f.manager, ctx, w)
	if err == nil || got != (reviewworkspace.Workspace{}) || time.Since(started) > time.Second {
		t.Fatal("cancelled reopen did not fail safely and promptly")
	}
	if _, err := openPrepared(f.manager, nil, w); err == nil {
		t.Fatal("nil context accepted")
	}
	var absent *reviewworkspace.Manager
	if _, err := openPrepared(absent, context.Background(), w); err == nil {
		t.Fatal("missing manager accepted")
	}
	if !reflect.DeepEqual(before, openFilesystemState(t, f.root)) {
		t.Fatal("failed reopen created or modified owned state")
	}
}

func TestOpenRejectsWrongPinsAndTamperingWithoutRepair(t *testing.T) {
	f := newFixture(t)
	for name, change := range map[string]func(*reviewworkspace.Workspace){
		"candidate":  func(w *reviewworkspace.Workspace) { w.CandidateSHA = f.base },
		"base":       func(w *reviewworkspace.Workspace) { w.BaseSHA = f.candidate },
		"merge_base": func(w *reviewworkspace.Workspace) { w.MergeBaseSHA = f.base },
		"hash":       func(w *reviewworkspace.Workspace) { w.PatchSHA256 = strings.Repeat("a", 64) },
		"canonical":  func(w *reviewworkspace.Workspace) { w.RepositoryCanonical = "other/repository" },
		"source": func(w *reviewworkspace.Workspace) {
			write(t, filepath.Join(w.Directory, "source.txt"), "tampered", 0600)
		},
		"origin": func(w *reviewworkspace.Workspace) {
			git(t, f.git, w.Directory, "remote", "set-url", "origin", "https://example.invalid/other.git")
		},
		"comparison": func(w *reviewworkspace.Workspace) {
			git(t, f.git, w.Directory, "update-ref", w.ComparisonRef, f.candidate)
		},
		"head": func(w *reviewworkspace.Workspace) { git(t, f.git, w.Directory, "update-ref", "HEAD", f.base) },
	} {
		t.Run(name, func(t *testing.T) {
			w := boundOpenFixture(t, f, "open_"+name)
			change(&w)
			before := openFilesystemState(t, w.Directory)
			got, err := openPrepared(f.manager, context.Background(), w)
			if err == nil || got != (reviewworkspace.Workspace{}) || strings.Contains(err.Error(), f.root) {
				t.Fatal("wrong identity accepted or private path disclosed")
			}
			if !reflect.DeepEqual(before, openFilesystemState(t, w.Directory)) {
				t.Fatal("failed reopen repaired or changed existing workspace")
			}
		})
	}
}

func TestOpenRejectsRedirectedOwnedWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink fixture")
	}
	f := newFixture(t)
	w := boundOpenFixture(t, f, "redirected")
	if err := os.Rename(w.Directory, w.Directory+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.source, w.Directory); err != nil {
		t.Fatal(err)
	}
	before := openFilesystemState(t, f.root)
	got, err := openPrepared(f.manager, context.Background(), w)
	if err == nil || got != (reviewworkspace.Workspace{}) {
		t.Fatal("redirected workspace adopted")
	}
	if !reflect.DeepEqual(before, openFilesystemState(t, f.root)) {
		t.Fatal("redirected workspace refusal changed state")
	}
}
