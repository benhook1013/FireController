package reviewworkspace_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func comparisonOriginal(f fixture) reviewworkspace.ComparisonSnapshot {
	return reviewworkspace.ComparisonSnapshot{BaseSHA: f.base, CandidateSHA: f.candidate}
}

func comparisonTree(t *testing.T, f fixture, commit string, change ...string) string {
	t.Helper()
	git(t, f.git, f.cache, "read-tree", commit)
	git(t, f.git, f.cache, append([]string{"--git-dir=" + f.cache, "--work-tree=" + f.source, "update-index"}, change...)...)
	return git(t, f.git, f.cache, "write-tree")
}

func comparisonGraph(t *testing.T, f fixture, ancestorTree, baseTree, headTree string) reviewworkspace.ComparisonSnapshot {
	t.Helper()
	ancestor := git(t, f.git, f.cache, "commit-tree", ancestorTree, "-m", "new graph ancestor")
	return reviewworkspace.ComparisonSnapshot{
		BaseSHA:      git(t, f.git, f.cache, "commit-tree", baseTree, "-p", ancestor, "-m", "new graph base"),
		CandidateSHA: git(t, f.git, f.cache, "commit-tree", headTree, "-p", ancestor, "-m", "new graph head"),
	}
}

func comparisonFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var data []byte
		if !entry.IsDir() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		files[path] = fmt.Sprintf("%v:%x", info.Mode(), sha256.Sum256(data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestComparisonEquivalenceGraphOnlyMovementPreservesAllSourceState(t *testing.T) {
	f := newFixture(t)
	w := prepare(t, f, "original_comparison")
	ancestorTree := git(t, f.git, f.cache, "rev-parse", f.ancestor+"^{tree}")
	baseTree := git(t, f.git, f.cache, "rev-parse", f.base+"^{tree}")
	headTree := git(t, f.git, f.cache, "rev-parse", f.candidate+"^{tree}")
	current := comparisonGraph(t, f, ancestorTree, baseTree, headTree)
	original := comparisonOriginal(f)
	before := comparisonFiles(t, filepath.Dir(f.root))
	proof, err := f.manager.VerifyComparisonEquivalence(context.Background(), 42, original, current, w.PatchSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if current.BaseSHA == original.BaseSHA || current.CandidateSHA == original.CandidateSHA || proof.OriginalBaseTree != baseTree || proof.CurrentBaseTree != baseTree || proof.OriginalHeadTree != headTree || proof.CurrentHeadTree != headTree || proof.OriginalMergeBaseSHA != f.ancestor || proof.CurrentMergeBaseSHA == f.ancestor || proof.PatchSHA256 != w.PatchSHA256 {
		t.Fatalf("wrong equivalence proof: %#v", proof)
	}
	inventory := git(t, f.git, f.cache, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--raw", "--no-abbrev", "--no-renames", "-z", f.ancestor, f.candidate, "--")
	hash := sha256.Sum256([]byte(inventory))
	if proof.InventorySHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("inventory proof does not cover the complete canonical raw inventory")
	}
	if !reflect.DeepEqual(before, comparisonFiles(t, filepath.Dir(f.root))) {
		t.Fatal("read-only equivalence check changed source, cache, refs or workspaces")
	}
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal("original workspace changed:", err)
	}
}

func TestComparisonEquivalenceRejectsWholeTreeAndComparisonChanges(t *testing.T) {
	f := newFixture(t)
	w := prepare(t, f, "original_comparison")
	original := comparisonOriginal(f)
	ancestorTree := git(t, f.git, f.cache, "rev-parse", f.ancestor+"^{tree}")
	baseTree := git(t, f.git, f.cache, "rev-parse", f.base+"^{tree}")
	headTree := git(t, f.git, f.cache, "rev-parse", f.candidate+"^{tree}")
	blob := git(t, f.git, f.cache, "rev-parse", f.candidate+":source.txt")
	contextPath := filepath.Join(t.TempDir(), "context")
	write(t, contextPath, "unrelated context\n", 0o600)
	contextBlob := git(t, f.git, f.cache, "hash-object", "-w", "--", contextPath)
	addContext := []string{"--add", "--cacheinfo", "100644," + contextBlob + ",unrelated.txt"}
	changedContext := comparisonGraph(t, f,
		comparisonTree(t, f, ancestorTree, addContext...),
		comparisonTree(t, f, baseTree, addContext...),
		comparisonTree(t, f, headTree, addContext...))
	// The selected diff really is identical; whole-tree equality must still refuse it.
	mergeBase := git(t, f.git, f.cache, "merge-base", changedContext.BaseSHA, changedContext.CandidateSHA)
	patch := git(t, f.git, f.cache, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--binary", "--full-index", "--no-renames", "--diff-algorithm=myers", "--src-prefix=a/", "--dst-prefix=b/", mergeBase, changedContext.CandidateSHA, "--") + "\n"
	patchHash := sha256.Sum256([]byte(patch))
	if hex.EncodeToString(patchHash[:]) != w.PatchSHA256 {
		t.Fatal("changed-context fixture did not preserve the selected patch")
	}
	changedMergeBase := reviewworkspace.ComparisonSnapshot{
		BaseSHA:      git(t, f.git, f.cache, "commit-tree", baseTree, "-p", f.base, "-m", "new base ancestry"),
		CandidateSHA: git(t, f.git, f.cache, "commit-tree", headTree, "-p", f.base, "-m", "new head ancestry"),
	}
	for name, current := range map[string]reviewworkspace.ComparisonSnapshot{
		"unrelated-context-same-patch": changedContext,
		"changed-merge-base-patch":     changedMergeBase,
		"head-content":                 {BaseSHA: f.base, CandidateSHA: f.ancestor},
		"head-mode": comparisonGraph(t, f, ancestorTree, baseTree,
			comparisonTree(t, f, headTree, "--cacheinfo", "100755,"+blob+",source.txt")),
		"head-deletion": comparisonGraph(t, f, ancestorTree, baseTree,
			comparisonTree(t, f, headTree, "--force-remove", "source.txt")),
		"head-scope": comparisonGraph(t, f, ancestorTree, baseTree,
			comparisonTree(t, f, headTree, "--add", "--cacheinfo", "100644,"+blob+",additional.txt")),
	} {
		t.Run(name, func(t *testing.T) {
			proof, err := f.manager.VerifyComparisonEquivalence(context.Background(), 42, original, current, w.PatchSHA256)
			if err == nil || proof != (reviewworkspace.ComparisonEquivalence{}) || strings.Contains(err.Error(), f.root) {
				t.Fatal("different comparison accepted or partial/private proof returned", proof, err)
			}
		})
	}
}

func TestComparisonEquivalenceInventoryIncludesIgnoredSubmodules(t *testing.T) {
	f := newFixture(t)
	baseTree := comparisonTree(t, f, f.ancestor, "--add", "--cacheinfo", "160000,"+f.ancestor+",vendor")
	base := git(t, f.git, f.cache, "commit-tree", baseTree, "-p", f.ancestor, "-m", "gitlink base")
	headTree := comparisonTree(t, f, baseTree, "--cacheinfo", "160000,"+f.base+",vendor")
	head := git(t, f.git, f.cache, "commit-tree", headTree, "-p", base, "-m", "gitlink head")
	git(t, f.git, f.cache, "config", "diff.ignoreSubmodules", "all")
	git(t, f.git, f.cache, "config", "diff.relative", "true")
	// Keep patch()'s exact established semantics while requiring the independent
	// raw inventory to include every entry, even one hidden by local diff config.
	emptyPatch := sha256.Sum256(nil)
	snapshot := reviewworkspace.ComparisonSnapshot{BaseSHA: base, CandidateSHA: head}
	proof, err := f.manager.VerifyComparisonEquivalence(context.Background(), 42, snapshot, snapshot, hex.EncodeToString(emptyPatch[:]))
	if err != nil {
		t.Fatal(err)
	}
	inventory := git(t, f.git, f.cache, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-relative", "--ignore-submodules=none", "--raw", "--no-abbrev", "--no-renames", "-z", base, head, "--")
	hash := sha256.Sum256([]byte(inventory))
	if !strings.Contains(inventory, "vendor\x00") || proof.InventorySHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("raw inventory omitted a gitlink hidden by local diff config")
	}
}

func TestComparisonEquivalenceRejectsInvalidAndMissingObjects(t *testing.T) {
	f := newFixture(t)
	w := prepare(t, f, "original_comparison")
	original := comparisonOriginal(f)
	for _, id := range []int64{0, -1, 43} {
		if proof, err := f.manager.VerifyComparisonEquivalence(context.Background(), id, original, original, w.PatchSHA256); err == nil || proof != (reviewworkspace.ComparisonEquivalence{}) {
			t.Fatal("invalid or unprovisioned repository accepted")
		}
	}
	foreign := git(t, f.git, f.source, "commit-tree", git(t, f.git, f.source, "rev-parse", f.candidate+"^{tree}"), "-p", f.candidate, "-m", "foreign local-only object")
	for _, invalid := range []string{"main", "", strings.ToUpper(f.base), f.base[:39], strings.Repeat("0", 40), foreign, git(t, f.git, f.cache, "rev-parse", f.base+"^{tree}")} {
		for field := 0; field < 4; field++ {
			a, b := original, original
			*[]*string{&a.BaseSHA, &a.CandidateSHA, &b.BaseSHA, &b.CandidateSHA}[field] = invalid
			if proof, err := f.manager.VerifyComparisonEquivalence(context.Background(), 42, a, b, w.PatchSHA256); err == nil || proof != (reviewworkspace.ComparisonEquivalence{}) {
				t.Fatal("invalid, foreign, missing or noncommit object accepted")
			}
		}
	}
	for _, invalid := range []string{"", w.PatchSHA256[:63], strings.ToUpper(w.PatchSHA256), strings.Repeat("0", 64)} {
		if _, err := f.manager.VerifyComparisonEquivalence(context.Background(), 42, original, original, invalid); err == nil {
			t.Fatal("invalid or mismatched original patch hash accepted")
		}
	}
	if _, err := (*reviewworkspace.Manager)(nil).VerifyComparisonEquivalence(context.Background(), 42, original, original, w.PatchSHA256); err == nil {
		t.Fatal("nil manager accepted")
	}
	if _, err := f.manager.VerifyComparisonEquivalence(nil, 42, original, original, w.PatchSHA256); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.VerifyComparisonEquivalence(ctx, 42, original, original, w.PatchSHA256); err == nil {
		t.Fatal("cancelled caller accepted")
	}
	// Remove only the disposable cache copy of one synthetic blob. The source's
	// hardlinked object remains intact and cannot silently supply the missing data.
	blob := git(t, f.git, f.cache, "rev-parse", f.candidate+":source.txt")
	if err := os.Remove(filepath.Join(f.cache, "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if proof, err := f.manager.VerifyComparisonEquivalence(context.Background(), 42, original, original, w.PatchSHA256); err == nil || proof != (reviewworkspace.ComparisonEquivalence{}) {
		t.Fatal("missing reachable blob accepted")
	}
}

func TestComparisonEquivalenceRejectsMalformedIncompleteAndOversizedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controlled Git wrapper requires executable scripts")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("controlled Git wrapper requires Python")
	}
	f := newFixture(t)
	w := prepare(t, f, "original_comparison")
	original := comparisonOriginal(f)
	for _, mode := range []string{"malformed-raw", "changed-inventory", "oversized-raw", "malformed-tree", "cancel-command"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			wrapper, counter := filepath.Join(dir, "git-probe"), filepath.Join(dir, "raw-count")
			body := fmt.Sprintf(`#!%s
import os,subprocess,sys,time
from pathlib import Path
a=sys.argv[1:]
mode=%q
if os.environ.get('GIT_NO_LAZY_FETCH')!='1' or os.environ.get('GIT_ALLOW_PROTOCOL')!='' or os.environ.get('GIT_DIR')!=%q: sys.exit(90)
if mode=='cancel-command': time.sleep(60)
if mode=='malformed-tree' and any(x.endswith('^{tree}') for x in a):
 sys.stdout.write('invalid-tree\n');sys.exit(0)
if '--raw' in a:
 if mode=='malformed-raw': sys.stdout.buffer.write(b'invalid\0');sys.exit(0)
 if mode=='oversized-raw': sys.stdout.buffer.write(b'x'*((32<<20)+1));sys.exit(0)
 if mode=='changed-inventory':
  p=Path(%q); second=p.exists();p.write_text('seen')
  data=subprocess.check_output([%q]+a)
  sys.stdout.buffer.write(data.replace(b'source.txt\0',b'other.txt\0') if second else data);sys.exit(0)
os.execv(%q,[%q]+a)
`, python, mode, f.cache, counter, f.git, f.git, f.git)
			write(t, wrapper, body, 0o700)
			manager, err := reviewworkspace.New(f.root, wrapper)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if mode == "cancel-command" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			started := time.Now()
			proof, err := manager.VerifyComparisonEquivalence(ctx, 42, original, original, w.PatchSHA256)
			if err == nil || proof != (reviewworkspace.ComparisonEquivalence{}) || time.Since(started) > 6*time.Second || strings.Contains(err.Error(), f.root) {
				t.Fatal("malformed/partial output or cancellation produced proof", proof, err)
			}
		})
	}
}
