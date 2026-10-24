package reviewruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

type applicabilityRuntimeFixture struct {
	manager                                *reviewworkspace.Manager
	verifier                               *ApplicabilityVerifier
	git, source, root, log, block, started string
	original                               reviewexec.Anchor
	current, changedContext                reviewexec.SourcePlan
	baseTree, headTree, inventory          string
}

func applicabilityGit(t *testing.T, executable, directory string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.autocrlf=false"}, args...)...)
	command.Dir = directory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Synthetic", "GIT_AUTHOR_EMAIL=synthetic@example.invalid", "GIT_COMMITTER_NAME=Synthetic", "GIT_COMMITTER_EMAIL=synthetic@example.invalid", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}
	data, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated applicability Git failed: %v %s", err, data)
	}
	return data
}

func newApplicabilityRuntimeFixture(t *testing.T) applicabilityRuntimeFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("controlled Git wrapper requires POSIX shell")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("isolated Git fixture requires Git")
	}
	dir := t.TempDir()
	f := applicabilityRuntimeFixture{git: git, source: filepath.Join(dir, "source"), root: filepath.Join(dir, "owned"), log: filepath.Join(dir, "calls"), block: filepath.Join(dir, "block"), started: filepath.Join(dir, "started")}
	run := func(args ...string) string {
		return strings.TrimSuffix(string(applicabilityGit(t, git, f.source, args...)), "\n")
	}
	if err := os.Mkdir(f.source, 0700); err != nil {
		t.Fatal(err)
	}
	run("init", "--quiet", "--template=", "--initial-branch=main")
	file := filepath.Join(f.source, "reviewed.txt")
	if err := os.WriteFile(file, []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "reviewed.txt")
	run("commit", "--quiet", "-m", "original base")
	base := run("rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("commit", "--quiet", "-am", "original candidate")
	head := run("rev-parse", "HEAD")
	f.baseTree = run("rev-parse", base+"^{tree}")
	f.headTree = run("rev-parse", head+"^{tree}")
	newBase := run("commit-tree", f.baseTree, "-m", "equivalent new base graph")
	newHead := run("commit-tree", f.headTree, "-p", newBase, "-m", "equivalent new head graph")
	patchArgs := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--binary", "--full-index", "--no-renames", "--diff-algorithm=myers", "--src-prefix=a/", "--dst-prefix=b/", base, head, "--"}
	patch := sha256.Sum256(applicabilityGit(t, git, f.source, patchArgs...))
	inventory := sha256.Sum256(applicabilityGit(t, git, f.source, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-relative", "--ignore-submodules=none", "--raw", "--no-abbrev", "--no-renames", "-z", base, head, "--"))
	f.inventory = hex.EncodeToString(inventory[:])
	f.original = reviewexec.Anchor{RepositoryID: 42, Repository: "owner/repo", PullNumber: 2, CandidateSHA: head, BaseSHA: base, ParentSHA: base, BaseRef: "main", MergeBaseSHA: base, PatchSHA256: hex.EncodeToString(patch[:]), ObservedAt: "2026-10-23T12:00:00Z"}
	f.current = reviewexec.SourcePlan{Repository: reviewprovider.Repository{ID: 42, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 42, State: "open", HeadSHA: newHead, BaseSHA: newBase, BaseRef: "main"}, ParentSHA: newBase, ObservedAt: "2026-10-23T12:01:00Z"}
	// Preserve the entire selected patch while changing unrelated whole-tree
	// context on both sides. This must not inherit the original review's proof.
	contextFile := filepath.Join(dir, "unrelated-context")
	if err := os.WriteFile(contextFile, []byte("unrelated context\n"), 0600); err != nil {
		t.Fatal(err)
	}
	blob := run("hash-object", "-w", "--", contextFile)
	contextTree := func(tree string) string {
		run("read-tree", tree)
		run("update-index", "--add", "--cacheinfo", "100644,"+blob+",unrelated.txt")
		return run("write-tree")
	}
	contextBase := run("commit-tree", contextTree(f.baseTree), "-m", "different whole base context")
	contextHead := run("commit-tree", contextTree(f.headTree), "-p", contextBase, "-m", "different whole head context")
	run("read-tree", "HEAD")
	f.changedContext = f.current
	f.changedContext.Pull.BaseSHA = contextBase
	f.changedContext.ParentSHA = contextBase
	f.changedContext.Pull.HeadSHA = contextHead
	changedPatch := append([]string(nil), patchArgs...)
	changedPatch[len(changedPatch)-3] = contextBase
	changedPatch[len(changedPatch)-2] = contextHead
	changedHash := sha256.Sum256(applicabilityGit(t, git, f.source, changedPatch...))
	if changedHash != patch {
		t.Fatal("whole-tree fixture changed the selected patch")
	}
	wrapper := filepath.Join(dir, "controlled-git")
	script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = fetch ]; then printf 'blocked-fetch\\n' >> " + cliShellQuote(f.log) + "; exit 93; fi\ndone\nprintf 'local\\n' >> " + cliShellQuote(f.log) + "\nif [ -f " + cliShellQuote(f.block) + " ]; then : > " + cliShellQuote(f.started) + "; while :; do sleep 1; done; fi\nexec " + cliShellQuote(git) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.manager, err = reviewworkspace.New(f.root, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := f.manager.CachePath(42)
	applicabilityGit(t, git, dir, "init", "--quiet", "--bare", "--template=", cache)
	if err := os.Chmod(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(cache, "objects"), os.DirFS(filepath.Join(f.source, ".git", "objects"))); err != nil {
		t.Fatal(err)
	}
	f.verifier, err = NewApplicabilityVerifier(f.manager, "synthetic-private-token")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f applicabilityRuntimeFixture) calls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplicabilityRuntimeEquivalentObjectsRenameAndStackedParent(t *testing.T) {
	f := newApplicabilityRuntimeFixture(t)
	before := applicabilityGit(t, f.git, f.source, "status", "--porcelain=v1")
	for _, mode := range []string{"original-name", "renamed", "stacked"} {
		t.Run(mode, func(t *testing.T) {
			original, current := f.original, f.current
			if mode == "renamed" {
				original.Repository = "previous-owner/previous-name"
			}
			if mode == "stacked" {
				original.ParentPullNumber = 7
				current.ParentPullNumber = 7
			}
			originalBefore, currentBefore := original, current
			proof, err := f.verifier.VerifyReviewApplicability(t.Context(), original, current)
			if err != nil || proof.OriginalBaseTree != f.baseTree || proof.CurrentBaseTree != f.baseTree || proof.OriginalHeadTree != f.headTree || proof.CurrentHeadTree != f.headTree || proof.OriginalMergeBaseSHA != original.BaseSHA || proof.CurrentMergeBaseSHA != current.Pull.BaseSHA || proof.PatchSHA256 != original.PatchSHA256 || proof.InventorySHA256 != f.inventory {
				t.Fatalf("wrong equivalence proof: %+v %v", proof, err)
			}
			if original.BaseSHA == current.Pull.BaseSHA || original.CandidateSHA == current.Pull.HeadSHA || !reflect.DeepEqual(original, originalBefore) || !reflect.DeepEqual(current, currentBefore) {
				t.Fatal("fixture did not exercise distinct immutable source identities")
			}
		})
	}
	if strings.Contains(f.calls(t), "blocked-fetch") {
		t.Fatal("complete pinned cache attempted network fetch")
	}
	if after := applicabilityGit(t, f.git, f.source, "status", "--porcelain=v1"); !reflect.DeepEqual(before, after) {
		t.Fatal("equivalence modified source checkout")
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "workspaces"))
	if err != nil || len(entries) != 0 {
		t.Fatal("verifier materialized a review workspace", entries, err)
	}
	cache, _ := f.manager.CachePath(42)
	config, err := os.ReadFile(filepath.Join(cache, "config"))
	if err != nil || strings.Contains(string(config), "synthetic-private-token") || strings.Contains(string(config), "github.com") {
		t.Fatal("cache persisted credentials or remote configuration", err)
	}
}

func TestApplicabilityRuntimeRejectsScopeObjectsAndPatchMismatch(t *testing.T) {
	f := newApplicabilityRuntimeFixture(t)
	for name, change := range map[string]func(*reviewexec.Anchor, *reviewexec.SourcePlan){
		"foreign-stable-repository": func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { a.RepositoryID++ },
		"foreign-pull-repository":   func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { p.Pull.RepositoryID++ },
		"foreign-pull":              func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { p.Pull.Number++ },
		"original-parent-mismatch":  func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { a.ParentSHA = a.CandidateSHA },
		"current-parent-mismatch":   func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { p.ParentSHA = p.Pull.HeadSHA },
		"changed-head-tree":         func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { p.Pull.HeadSHA = p.Pull.BaseSHA },
		"changed-base-tree": func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) {
			p.Pull.BaseSHA = p.Pull.HeadSHA
			p.ParentSHA = p.Pull.BaseSHA
		},
		"same-patch-different-whole-context": func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { *p = f.changedContext },
		"original-patch-mismatch":            func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { a.PatchSHA256 = strings.Repeat("0", 64) },
		"missing-current-object":             func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { p.Pull.HeadSHA = strings.Repeat("f", 40) },
		"noncommit-current-object":           func(a *reviewexec.Anchor, p *reviewexec.SourcePlan) { p.Pull.HeadSHA = f.headTree },
	} {
		t.Run(name, func(t *testing.T) {
			a, p := f.original, f.current
			change(&a, &p)
			before := f.calls(t)
			proof, err := f.verifier.VerifyReviewApplicability(t.Context(), a, p)
			if err == nil || proof != (reviewexec.EquivalentComparison{}) || strings.Contains(err.Error(), f.root) || strings.Contains(err.Error(), "synthetic-private-token") {
				t.Fatal("unsafe equivalence result", proof, err)
			}
			if strings.HasPrefix(name, "foreign-") || strings.HasSuffix(name, "parent-mismatch") {
				if f.calls(t) != before {
					t.Fatal("invalid source scope reached Git")
				}
			}
		})
	}
}

func TestApplicabilityRuntimeConstructorAndCancellation(t *testing.T) {
	f := newApplicabilityRuntimeFixture(t)
	if _, err := NewApplicabilityVerifier(nil, ""); err == nil {
		t.Fatal("nil manager accepted")
	}
	for i, credential := range []string{"token with-space", "token\ncontrol", strings.Repeat("x", maxCredentialTokenBytes+1), string([]byte{0xff})} {
		if _, err := NewApplicabilityVerifier(f.manager, credential); err == nil {
			t.Fatal("invalid verifier credential accepted", i)
		}
	}
	if _, err := NewApplicabilityVerifier(f.manager, ""); err != nil {
		t.Fatal("public-cache credential default rejected", err)
	}
	if proof, err := (*ApplicabilityVerifier)(nil).VerifyReviewApplicability(t.Context(), f.original, f.current); err == nil || proof != (reviewexec.EquivalentComparison{}) {
		t.Fatal("nil verifier accepted", proof, err)
	}
	if proof, err := f.verifier.VerifyReviewApplicability(nil, f.original, f.current); err == nil || proof != (reviewexec.EquivalentComparison{}) {
		t.Fatal("nil context accepted", proof, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := f.calls(t)
	if proof, err := f.verifier.VerifyReviewApplicability(ctx, f.original, f.current); !errors.Is(err, context.Canceled) || proof != (reviewexec.EquivalentComparison{}) || f.calls(t) != before {
		t.Fatal("canceled verifier reached Git or returned proof", proof, err)
	}
	if err := os.WriteFile(f.block, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		proof, err := f.verifier.VerifyReviewApplicability(ctx, f.original, f.current)
		if proof != (reviewexec.EquivalentComparison{}) {
			err = fmt.Errorf("canceled verifier returned proof: %+v", proof)
		}
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(f.started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controlled Git did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("in-flight cancellation lost its classification", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled verifier retained Git work")
	}
}
