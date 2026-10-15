package reviewworkspace_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

type fixture struct {
	manager                   *reviewworkspace.Manager
	git, source, cache, root  string
	ancestor, base, candidate string
}

func git(t *testing.T, executable, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.autocrlf=false"}, args...)...)
	command.Dir = directory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "SystemRoot=" + os.Getenv("SystemRoot"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Synthetic", "GIT_AUTHOR_EMAIL=synthetic@example.invalid", "GIT_COMMITTER_NAME=Synthetic", "GIT_COMMITTER_EMAIL=synthetic@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}
	value, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic git %v failed: %v: %s", args, err, value)
	}
	return strings.TrimSuffix(string(value), "\n")
}

func write(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	executable, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("real Git is required for materialization proof")
	}
	temp := t.TempDir()
	f := fixture{git: executable, source: filepath.Join(temp, "source"), root: filepath.Join(temp, "owned")}
	if err := os.Mkdir(f.source, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, f.git, f.source, "init", "--template=", "--initial-branch=main")
	write(t, filepath.Join(f.source, "source.txt"), "ancestor\n", 0o600)
	write(t, filepath.Join(f.source, ".gitattributes"), "*.txt filter=evil\n", 0o600)
	write(t, filepath.Join(f.source, ".gitignore"), "ignored.tmp\n", 0o600)
	git(t, f.git, f.source, "add", "--", "source.txt", ".gitattributes", ".gitignore")
	git(t, f.git, f.source, "commit", "-m", "synthetic ancestor")
	f.ancestor = git(t, f.git, f.source, "rev-parse", "HEAD")
	write(t, filepath.Join(f.source, "base-only.txt"), "base branch\n", 0o600)
	git(t, f.git, f.source, "add", "--", "base-only.txt")
	git(t, f.git, f.source, "commit", "-m", "synthetic base")
	f.base = git(t, f.git, f.source, "rev-parse", "HEAD")
	// Candidate has a different parent from base; merge-base must be computed.
	write(t, filepath.Join(f.source, "source.txt"), "candidate\n", 0o600)
	git(t, f.git, f.source, "add", "--", "source.txt")
	tree := git(t, f.git, f.source, "write-tree")
	f.candidate = git(t, f.git, f.source, "commit-tree", tree, "-p", f.ancestor, "-m", "synthetic candidate")
	git(t, f.git, f.source, "update-ref", "refs/heads/candidate", f.candidate)
	f.manager, err = reviewworkspace.New(f.root, f.git)
	if err != nil {
		t.Fatal(err)
	}
	f.cache, err = f.manager.CachePath(42)
	if err != nil {
		t.Fatal(err)
	}
	git(t, f.git, temp, "clone", "--bare", "--template=", "--", f.source, f.cache)
	if err := os.Chmod(f.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func prepare(t *testing.T, f fixture, attempt string) reviewworkspace.Workspace {
	t.Helper()
	w, err := f.manager.Prepare(context.Background(), 42, attempt, f.candidate, f.base)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPinnedDetachedSourceComparisonAndPatch(t *testing.T) {
	f := newFixture(t)
	w := prepare(t, f, "attempt_1")
	if w.CandidateSHA != f.candidate || w.BaseSHA != f.base || w.MergeBaseSHA != f.ancestor || w.ComparisonRef != "refs/firecontroller/review-base/attempt_1" {
		t.Fatalf("wrong admitted identities: %#v", w)
	}
	if value := git(t, f.git, w.Directory, "rev-parse", "HEAD"); value != f.candidate {
		t.Fatal("candidate moved")
	}
	if value := git(t, f.git, w.Directory, "rev-parse", w.ComparisonRef); value != f.base {
		t.Fatal("comparison ref moved")
	}
	content, err := os.ReadFile(filepath.Join(w.Directory, "source.txt"))
	if err != nil || string(content) != "candidate\n" {
		t.Fatal("source bytes differ")
	}
	diff := git(t, f.git, f.cache, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--binary", "--full-index", "--no-renames", "--diff-algorithm=myers", "--src-prefix=a/", "--dst-prefix=b/", f.ancestor, f.candidate, "--") + "\n"
	hash := sha256.Sum256([]byte(diff))
	if w.PatchSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("patch hash does not cover exact deterministic diff bytes")
	}
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	// Move cache branches after capture, including the default branch. Full SHAs stay pinned.
	newCommit := git(t, f.git, f.cache, "commit-tree", git(t, f.git, f.cache, "rev-parse", f.candidate+"^{tree}"), "-p", f.candidate, "-m", "new synthetic head")
	git(t, f.git, f.cache, "update-ref", "refs/heads/main", newCommit)
	git(t, f.git, f.cache, "update-ref", "refs/heads/candidate", newCommit)
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal("branch movement changed pinned workspace:", err)
	}
	second := prepare(t, f, "attempt_2")
	write(t, filepath.Join(w.Directory, "source.txt"), "dirty first attempt\n", 0o600)
	if err := f.manager.Verify(context.Background(), second); err != nil {
		t.Fatal("one attempt contaminated another:", err)
	}
	if _, err := f.manager.Prepare(context.Background(), 42, "attempt_1", f.candidate, f.base); err == nil || err.Error() != "review workspace already exists" {
		t.Fatal("existing attempt must conflict")
	}
	unchanged, _ := os.ReadFile(filepath.Join(w.Directory, "source.txt"))
	if string(unchanged) != "dirty first attempt\n" {
		t.Fatal("conflict overwrote existing attempt")
	}
}

func TestVerifyRejectsChangedWorkspace(t *testing.T) {
	f := newFixture(t)
	for name, change := range map[string]func(*reviewworkspace.Workspace){
		"tracked": func(w *reviewworkspace.Workspace) {
			write(t, filepath.Join(w.Directory, "source.txt"), "changed", 0o600)
		},
		"untracked": func(w *reviewworkspace.Workspace) { write(t, filepath.Join(w.Directory, "extra"), "extra", 0o600) },
		"ignored": func(w *reviewworkspace.Workspace) {
			write(t, filepath.Join(w.Directory, "ignored.tmp"), "ignored", 0o600)
		},
		"comparison": func(w *reviewworkspace.Workspace) {
			git(t, f.git, w.Directory, "update-ref", w.ComparisonRef, f.candidate)
		},
		"attached": func(w *reviewworkspace.Workspace) {
			git(t, f.git, w.Directory, "update-ref", "refs/heads/attached", f.candidate)
			git(t, f.git, w.Directory, "symbolic-ref", "HEAD", "refs/heads/attached")
		},
		"candidate":  func(w *reviewworkspace.Workspace) { w.CandidateSHA = f.base },
		"merge_base": func(w *reviewworkspace.Workspace) { w.MergeBaseSHA = f.base },
		"hash":       func(w *reviewworkspace.Workspace) { w.PatchSHA256 = strings.Repeat("a", 64) },
		"directory":  func(w *reviewworkspace.Workspace) { w.Directory = f.source },
		"reference":  func(w *reviewworkspace.Workspace) { w.ComparisonRef = "refs/heads/main" },
	} {
		t.Run(name, func(t *testing.T) {
			w := prepare(t, f, name)
			change(&w)
			if err := f.manager.Verify(context.Background(), w); err == nil || strings.Contains(err.Error(), f.root) {
				t.Fatal("changed workspace was accepted or disclosed private path")
			}
		})
	}
}

func TestConcurrentAttemptCreationHasOneOwner(t *testing.T) {
	f := newFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := f.manager.Prepare(context.Background(), 42, "same_attempt", f.candidate, f.base)
			results <- err
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first == nil && second == nil || first != nil && second != nil {
		t.Fatal("exactly one attempt creator must succeed")
	}
	conflict := first
	if conflict == nil {
		conflict = second
	}
	if conflict.Error() != "review workspace already exists" {
		t.Fatal("second creator did not report a safe conflict")
	}
}

func TestInvalidInputsAndCachesHaveNoAttemptEffects(t *testing.T) {
	f := newFixture(t)
	for _, id := range []int64{0, -1} {
		if _, err := f.manager.CachePath(id); err == nil {
			t.Fatal("invalid repository identity accepted")
		}
	}
	for _, id := range []string{"", "..", "a/b", "a\\b", "a b", "é", strings.Repeat("x", 129)} {
		if _, err := f.manager.Prepare(context.Background(), 42, id, f.candidate, f.base); err == nil {
			t.Fatal("invalid attempt accepted")
		}
	}
	for _, sha := range []string{"main", strings.ToUpper(f.candidate), f.candidate[:39], strings.Repeat("a", 40)} {
		if _, err := f.manager.Prepare(context.Background(), 42, "invalid_sha", sha, f.base); err == nil {
			t.Fatal("bad or missing object accepted")
		}
	}
	if _, err := f.manager.Prepare(context.Background(), 43, "missing_repo", f.candidate, f.base); err == nil {
		t.Fatal("missing cache fell back to another repository")
	}
	nonbare, _ := f.manager.CachePath(44)
	git(t, f.git, filepath.Dir(f.root), "clone", "--no-checkout", "--template=", "--", f.source, nonbare)
	if err := os.Chmod(nonbare, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Prepare(context.Background(), 44, "nonbare", f.candidate, f.base); err == nil {
		t.Fatal("non-bare cache accepted")
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "workspaces"))
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid selectors/caches created attempt state")
	}
	// CachePath remains a pure computation for a valid but unprovisioned ID.
	path, err := f.manager.CachePath(9223372036854775807)
	if err != nil || filepath.Base(path) != "9223372036854775807.git" {
		t.Fatal("repository path overflow")
	}
}

func TestPrivateLayoutAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission/symlink runtime proof; Windows compilation remains supported")
	}
	f := newFixture(t)
	for _, directory := range []string{f.root, filepath.Join(f.root, "repositories"), filepath.Join(f.root, "workspaces")} {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatal("owned directory is not private")
		}
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewworkspace.New(link, f.git); err == nil {
		t.Fatal("symlink root accepted")
	}
	if _, err := reviewworkspace.New(filepath.Join(link, "redirected-child"), f.git); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	if _, err := os.Stat(filepath.Join(f.root, "redirected-child")); !os.IsNotExist(err) {
		t.Fatal("symlink ancestor rejection created redirected state")
	}
	cacheLink, _ := f.manager.CachePath(45)
	if err := os.Symlink(f.cache, cacheLink); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Prepare(context.Background(), 45, "cache_link", f.candidate, f.base); err == nil {
		t.Fatal("symlink cache accepted")
	}
	workspaceLink := filepath.Join(f.root, "workspaces", "existing_link")
	if err := os.Symlink(f.source, workspaceLink); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Prepare(context.Background(), 42, "existing_link", f.candidate, f.base); err == nil {
		t.Fatal("symlink existing attempt accepted")
	}
	if err := os.Chmod(filepath.Join(f.root, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewworkspace.New(f.root, f.git); err == nil {
		t.Fatal("public owned parent accepted")
	}
}

func TestHooksFiltersTemplatesAndAmbientConfigurationAreNotInherited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based malicious fixture is Unix only")
	}
	f := newFixture(t)
	trap := filepath.Join(t.TempDir(), "executed")
	command := "sh -c 'printf altered; touch \"" + trap + "\"'"
	git(t, f.git, f.cache, "config", "filter.evil.smudge", command)
	git(t, f.git, f.cache, "config", "filter.evil.required", "true")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(hooks, "post-checkout"), "#!/bin/sh\ntouch \""+trap+"\"\nexit 1\n", 0o700)
	git(t, f.git, f.cache, "config", "core.hooksPath", hooks)
	template := filepath.Join(t.TempDir(), "template")
	if err := os.MkdirAll(filepath.Join(template, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(template, "hooks", "post-checkout"), "#!/bin/sh\ntouch \""+trap+"\"\nexit 1\n", 0o700)
	home := t.TempDir()
	global := filepath.Join(home, ".gitconfig")
	write(t, global, "[core]\n hooksPath = "+hooks+"\n[init]\n templateDir = "+template+"\n[filter \"evil\"]\n required = true\n smudge = "+command+"\n[diff]\n external = "+command+"\n", 0o600)
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_TEMPLATE_DIR", template)
	t.Setenv("GIT_EXTERNAL_DIFF", command)
	t.Setenv("GIT_DIR", f.cache)
	t.Setenv("GIT_WORK_TREE", f.source)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooks)
	manager, err := reviewworkspace.New(f.root, f.git)
	if err != nil {
		t.Fatal(err)
	}
	f.manager = manager
	w := prepare(t, f, "environment")
	if content, _ := os.ReadFile(filepath.Join(w.Directory, "source.txt")); string(content) != "candidate\n" {
		t.Fatal("filter or CRLF conversion altered source")
	}
	if _, err := os.Stat(trap); !os.IsNotExist(err) {
		t.Fatal("repository/global hook, filter, template or external diff executed")
	}
	if err := f.manager.Verify(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationAndErrorsAreBoundedAndSafe(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.Prepare(ctx, 42, "cancelled", f.candidate, f.base); err == nil {
		t.Fatal("cancelled caller accepted")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, "workspaces")); len(entries) != 0 {
		t.Fatal("pre-cancelled request created state")
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, mode := range []string{"slow", "error"} {
		t.Run(mode, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "git-probe")
			body := "#!/bin/sh\nprintf 'private-token-and-path' >&2\nexit 1\n"
			if mode == "slow" {
				body = "#!/bin/sh\nexec sleep 60\n"
			}
			write(t, script, body, 0o700)
			manager, err := reviewworkspace.New(f.root, script)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err = manager.Prepare(ctx, 42, "probe_"+mode, f.candidate, f.base)
			if err == nil || time.Since(started) > 2*time.Second || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), f.root) {
				t.Fatal("command cancellation/errors not finite and safe")
			}
		})
	}
}

func TestOversizedDiffIsRejectedWithoutTruncation(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.source, "large.txt"), strings.Repeat("z", (32<<20)+1)+"\n", 0o600)
	git(t, f.git, f.source, "add", "--", "large.txt")
	tree := git(t, f.git, f.source, "write-tree")
	large := git(t, f.git, f.source, "commit-tree", tree, "-p", f.ancestor, "-m", "oversized synthetic patch")
	// Transfer only local synthetic objects into the server cache, without a remote.
	git(t, f.git, f.cache, "fetch", "--no-tags", "--", f.source, large)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.manager.Prepare(ctx, 42, "oversized", large, f.base); err == nil {
		t.Fatal("oversized patch was silently hashed as a truncated success")
	}
	if _, err := os.Stat(filepath.Join(f.root, "workspaces", "oversized")); err != nil {
		t.Fatal("owned partial diagnostic state unexpectedly removed")
	}
}
