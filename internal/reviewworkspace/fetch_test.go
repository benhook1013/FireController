package reviewworkspace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const fetchCandidate = "0123456789abcdef0123456789abcdef01234567"
const fetchBase = "89abcdef0123456789abcdef0123456789abcdef"

func TestFetchRetriesEmptyCacheWithoutDiscoveringParentRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controlled Git wrapper requires Unix execution")
	}
	executable, err := exec.LookPath("git")
	if err != nil {
		t.Skip("real Git unavailable")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("controlled Git wrapper requires Python")
	}
	dir := t.TempDir()
	git := func(directory string, args ...string) string {
		t.Helper()
		command := exec.Command(executable, args...)
		command.Dir = directory
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		value, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("owned fixture Git: %v %s", err, value)
		}
		return strings.TrimSpace(string(value))
	}
	source, parent := filepath.Join(dir, "source"), filepath.Join(dir, "parent.git")
	git(dir, "init", "--quiet", "--template=", source)
	file := filepath.Join(source, "source.txt")
	if err := os.WriteFile(file, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(source, "add", "source.txt")
	git(source, "commit", "--quiet", "-m", "base")
	base := git(source, "rev-parse", "HEAD")
	git(dir, "clone", "--quiet", "--bare", "--template=", source, parent)
	if err := os.WriteFile(file, []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(source, "commit", "--quiet", "-am", "candidate")
	candidate := git(source, "rev-parse", "HEAD")
	flag, log := filepath.Join(dir, "failed-init"), filepath.Join(dir, "calls.jsonl")
	wrapper := filepath.Join(dir, "controlled-git")
	script := fmt.Sprintf(`#!%s
import json,os,sys
from pathlib import Path
a=sys.argv[1:]
with open(%q,'a') as log: log.write(json.dumps(a)+'\n')
if 'init' in a and not Path(%q).exists():
 Path(%q).touch()
 sys.exit(91)
if 'fetch' in a:
 i=a.index('fetch')
 if a[i:]!=['fetch','--no-tags','--no-recurse-submodules','--no-write-fetch-head','--','https://github.com/owner/repo.git',%q,%q]:sys.exit(92)
 a[i+5]=%q
 a=['-c','protocol.file.allow=always']+a
for arg in a:
 if '://' in arg:sys.exit(93)
os.execv(%q,[%q]+a)
`, python, log, flag, flag, candidate, base, source, executable, executable)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := New(filepath.Join(parent, "owned"), wrapper)
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := manager.CachePath(77)
	if err := manager.Fetch(context.Background(), 77, "owner/repo", candidate, base, ""); !errors.Is(err, errMaterialize) {
		t.Fatalf("injected initialization failure: %v", err)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed init did not leave the expected empty cache: %v %v", entries, err)
	}
	if err := manager.Fetch(context.Background(), 77, "owner/repo", candidate, base, ""); err != nil {
		t.Fatalf("empty cache retry failed: %v", err)
	}
	if git(dir, "--git-dir="+cache, "rev-parse", "--is-bare-repository") != "true" {
		t.Fatal("retry did not initialize the exact cache")
	}
	for _, sha := range []string{candidate, base} {
		if git(dir, "--git-dir="+cache, "rev-parse", "--verify", sha+"^{commit}") != sha {
			t.Fatal("cache lost its pinned source object")
		}
	}
	probe := exec.Command(executable, "--git-dir="+parent, "cat-file", "-e", candidate+"^{commit}")
	if probe.Run() == nil {
		t.Fatal("fetch inherited and modified its parent repository")
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var args []string
		if json.Unmarshal([]byte(line), &args) != nil {
			t.Fatal("invalid controlled Git log")
		}
		if suffixAfter(args, "init") == nil && !containsSequence(args, []string{"--git-dir=" + cache}) {
			t.Fatalf("cache command allowed parent discovery: %q", args)
		}
	}
}

type fetchFixture struct {
	manager           *Manager
	root, log, envlog string
	candidate, base   string
}

func newFetchFixture(t *testing.T, behavior string) fetchFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Git scripts require a Unix shell")
	}
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "fake-git")
	log := filepath.Join(dir, "args.log")
	envlog := filepath.Join(dir, "environment.log")
	provisioned := filepath.Join(dir, "fetch-complete")
	script := fmt.Sprintf(`printf 'CALL' >> %q
for arg do printf '|%%s' "$arg" >> %q; done
printf '\n' >> %q
printf '%%s|%%s|%%s\n' "${GIT_CONFIG_COUNT-unset}" "${GIT_CONFIG_KEY_0-unset}" "${GIT_CONFIG_VALUE_0-unset}" >> %q
case "$*" in
  *"init --bare"*) for cache do :; done; mkdir -p "$cache/objects" ;;
  *"rev-parse --is-bare-repository"*) printf 'true\n' ;;
  *"rev-parse --is-shallow-repository"*) printf 'false\n' ;;
  *"rev-parse --verify %s^{commit}"*) printf '%%s\n' %q ;;
  *"rev-parse --verify %s^{commit}"*) printf '%%s\n' %q ;;
  *"rev-list --objects"*) test -f %q ;;
  *"fetch --no-tags"*) %s; touch %q ;;
esac
`, log, log, log, envlog, fetchCandidate, fetchCandidate, fetchBase, fetchBase, provisioned, behavior, provisioned)
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nset -eu\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "private-data")
	manager, err := New(root, gitPath)
	if err != nil {
		t.Fatal(err)
	}
	return fetchFixture{manager: manager, root: root, log: log, envlog: envlog, candidate: fetchCandidate, base: fetchBase}
}

func (f fetchFixture) fetch(ctx context.Context, repositoryID int64, repo, candidate, base, token string) error {
	return f.manager.Fetch(ctx, repositoryID, repo, candidate, base, token)
}

func (f fetchFixture) calls(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	var result [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 1 || fields[0] != "CALL" {
			t.Fatalf("bad private fake-git record %q", line)
		}
		result = append(result, fields[1:])
	}
	return result
}

func TestFetchUsesFixedURLPinnedObjectsAndPrivateTokenEnvironment(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "88")
	t.Setenv("GIT_CONFIG_KEY_0", "ambient.key")
	t.Setenv("GIT_CONFIG_VALUE_0", "ambient-secret")
	f := newFetchFixture(t, ":")
	const token = "operator-private-token"
	if err := f.fetch(context.Background(), 41, "owner/repo", f.candidate, f.base, token); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	calls := f.calls(t)
	var initCall, fetchCall []string
	for _, call := range calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, " init ") {
			initCall = call
		}
		if strings.Contains(joined, " fetch ") {
			fetchCall = call
		}
	}
	if len(initCall) == 0 || len(fetchCall) == 0 {
		t.Fatalf("missing init/fetch commands: %#v", calls)
	}
	configPrefix := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.autocrlf=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "credential.helper=", "-c", "http.followRedirects=false"}
	for _, call := range calls {
		if !hasPrefix(call, configPrefix) {
			t.Fatalf("Git command omitted fixed isolation config: %q", call)
		}
		if suffixAfter(call, "init") == nil && !containsSequence(call, []string{"--git-dir=" + filepath.Join(f.root, "repositories", "41.git")}) {
			t.Fatalf("cache command omitted exact Git directory: %q", call)
		}
	}
	if !equalStrings(suffixAfter(initCall, "init"), []string{"init", "--bare", "--template=", "--", filepath.Join(f.root, "repositories", "41.git")}) {
		t.Fatalf("unexpected cache init args: %q", initCall)
	}
	want := []string{"fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", "https://github.com/owner/repo.git", f.candidate, f.base}
	if !equalStrings(suffixAfter(fetchCall, "fetch"), want) {
		t.Fatalf("fetch vector omitted or changed pinned inputs: %q", fetchCall)
	}
	for _, call := range calls {
		if strings.Contains(strings.Join(call, " "), token) || strings.Contains(strings.Join(call, " "), "Authorization") {
			t.Fatal("token or authorization was passed in argv")
		}
	}
	if len(calls) < 5 {
		t.Fatalf("expected init, bare check, fetch and two object checks; got %d", len(calls))
	}
	env, err := os.ReadFile(f.envlog)
	if err != nil {
		t.Fatal(err)
	}
	wantValue := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	if !strings.Contains(string(env), "1|http.extraHeader|"+wantValue) || strings.Contains(string(env), "ambient-secret") {
		t.Fatalf("token environment not isolated: %q", env)
	}
	if _, err := os.Stat(filepath.Join(f.root, "repositories", "41.git", "config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fake test unexpectedly wrote a Git config file: %v", err)
	}
	info, err := os.Stat(filepath.Join(f.root, "repositories", "41.git"))
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("cache directory is not private: info=%v err=%v", info, err)
	}
}

func TestFetchPublicModeNoTokenAndExistingCacheReuse(t *testing.T) {
	f := newFetchFixture(t, ":")
	if err := f.fetch(context.Background(), 42, "org_name/re.po", f.candidate, f.base, ""); err != nil {
		t.Fatal(err)
	}
	first := len(f.calls(t))
	if err := f.fetch(context.Background(), 42, "org_name/re.po", f.candidate, f.base, ""); err != nil {
		t.Fatalf("refresh existing cache: %v", err)
	}
	calls := f.calls(t)
	if first == 0 || len(calls) <= first {
		t.Fatalf("existing cache not refreshed: first=%d all=%d", first, len(calls))
	}
	for _, call := range calls[first:] {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "init --bare") {
			t.Fatalf("existing bare cache reinitialized: %q", call)
		}
	}
	env, err := os.ReadFile(f.envlog)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(env)), "\n") {
		if line == "" {
			continue
		}
		if line != "unset|unset|unset" {
			t.Fatalf("public fetch received token config: %q", line)
		}
	}
}

func TestFetchValidationDoesNotTouchCacheOrRunGit(t *testing.T) {
	f := newFetchFixture(t, ":")
	tests := []struct {
		name                         string
		id                           int64
		repo, candidate, base, token string
	}{
		{"bad id", 0, "owner/repo", f.candidate, f.base, ""},
		{"extra path", 5, "owner/repo/name", f.candidate, f.base, ""},
		{"url injection", 5, "owner/repo?x=1", f.candidate, f.base, ""},
		{"dot owner", 5, ".../repo", f.candidate, f.base, ""},
		{"bad sha", 5, "owner/repo", "HEAD", f.base, ""},
		{"token newline", 5, "owner/repo", f.candidate, f.base, "secret\nheader"},
		{"token control", 5, "owner/repo", f.candidate, f.base, "secret\x7f"},
		{"token too long", 5, "owner/repo", f.candidate, f.base, strings.Repeat("x", 257)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := f.fetch(context.Background(), test.id, test.repo, test.candidate, test.base, test.token); !errors.Is(err, errInput) {
				t.Fatalf("invalid input error=%v", err)
			}
		})
	}
	if _, err := os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid inputs invoked Git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "repositories")); err != nil {
		t.Fatalf("manager layout missing: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "repositories"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid inputs touched cache: %v %v", entries, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.fetch(canceled, 70, "owner/repo", f.candidate, f.base, ""); !errors.Is(err, errMaterialize) || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled fetch result: %v", err)
	}
	entries, err = os.ReadDir(filepath.Join(f.root, "repositories"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("pre-canceled fetch touched cache: %v %v", entries, err)
	}
}

func TestFixedBareInitializationVectorUsesOnlyTemporaryDirectory(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache.git")
	args := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.autocrlf=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "credential.helper=", "-c", "http.followRedirects=false", "init", "--bare", "--template=", "--", cache}
	command := exec.Command(gitPath, args...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("temporary bare init vector: %v %s", err, output)
	}
	verify := exec.Command(gitPath, "--git-dir", cache, "rev-parse", "--is-bare-repository")
	verify.Env = command.Env
	if output, err := verify.Output(); err != nil || strings.TrimSpace(string(output)) != "true" {
		t.Fatalf("temporary cache not bare: %v %q", err, output)
	}
	remote := exec.Command(gitPath, "--git-dir", cache, "remote")
	remote.Env = command.Env
	if output, err := remote.Output(); err != nil || len(output) != 0 {
		t.Fatalf("bare initialization unexpectedly configured a remote: %v %q", err, output)
	}
}

func TestFetchRejectsNonBareAndSymlinkCacheWithoutReplacing(t *testing.T) {
	dir := t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	root := filepath.Join(dir, "private-data")
	manager, err := New(root, gitPath)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "repositories", "43.git")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(cache, "preserved-invalid-state")
	if err := os.WriteFile(sentinel, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Fetch(context.Background(), 43, "owner/repo", fetchCandidate, fetchBase, ""); !errors.Is(err, errMaterialize) {
		t.Fatalf("non-bare cache result: %v", err)
	}
	entries, err := os.ReadDir(cache)
	value, readErr := os.ReadFile(sentinel)
	if err != nil || len(entries) != 1 || readErr != nil || string(value) != "preserve me" {
		t.Fatalf("nonempty invalid cache changed: %v %v %v", entries, err, readErr)
	}
	if runtime.GOOS != "windows" {
		f2 := newFetchFixture(t, ":")
		target := filepath.Join(f2.root, "do-not-touch")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(f2.root, "repositories", "44.git")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := f2.fetch(context.Background(), 44, "owner/repo", f2.candidate, f2.base, ""); !errors.Is(err, errConfiguration) {
			t.Fatalf("symlink cache result: %v", err)
		}
		if _, err := os.Stat(f2.log); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink cache invoked Git: %v", err)
		}
		entries, err := os.ReadDir(target)
		if err != nil || len(entries) != 0 {
			t.Fatalf("symlink target changed: %v %v", entries, err)
		}
	}
}

func TestFetchVerifiesExactFetchedCommitsWithoutFallback(t *testing.T) {
	f := newFetchFixture(t, ":")
	// The fake Git reports the right cache state but an unexpected object for the
	// candidate verification. Fetch must fail instead of looking up mutable refs.
	badScript := fmt.Sprintf(`printf 'CALL' >> %q
for arg do printf '|%%s' "$arg" >> %q; done; printf '\n' >> %q
case "$*" in *"rev-parse --is-bare-repository"*) echo true;; *"rev-parse --is-shallow-repository"*) echo false;; *"rev-parse --verify %s^{commit}"*) echo %s;; *"rev-parse --verify %s^{commit}"*) echo %s;; esac`, f.log, f.log, f.log, f.candidate, f.base, f.base, f.base)
	path := filepath.Join(f.root, "fake-replacement")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+badScript+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := New(f.root, path)
	if err != nil {
		t.Fatal(err)
	}
	f.manager = manager
	if err := f.fetch(context.Background(), 45, "owner/repo", f.candidate, f.base, ""); !errors.Is(err, errMaterialize) {
		t.Fatalf("unexpected object accepted: %v", err)
	}
	for _, call := range f.calls(t) {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "refs/heads") || strings.Contains(joined, "origin/") {
			t.Fatalf("mutable ref fallback used: %q", call)
		}
	}
}

func TestFetchCancellationBoundsAndCooperativeSerialization(t *testing.T) {
	f := newFetchFixture(t, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := f.fetch(ctx, 51, "owner/repo", f.candidate, f.base, ""); !errors.Is(err, errMaterialize) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled fetch result: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("canceled fetch exceeded bound: %s", time.Since(started))
	}

	f2 := newFetchFixture(t, "sleep 0.1")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f2.fetch(context.Background(), 52, "owner/repo", f2.candidate, f2.base, "")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent fetch %d: %v", i, err)
		}
	}
	var initCount, fetchCount int
	for _, call := range f2.calls(t) {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, " init --bare") {
			initCount++
		}
		if strings.Contains(joined, " fetch --no-tags") {
			fetchCount++
		}
	}
	if initCount != 1 || fetchCount != 1 {
		t.Fatalf("serialized shared cache operations init=%d fetch=%d calls=%q", initCount, fetchCount, f2.calls(t))
	}
}

func TestFetchBoundsBothOutputStreamsAndReturnsSafeFailures(t *testing.T) {
	large := strings.Repeat("x", fetchOutputLimit+1)
	for _, test := range []struct{ name, script string }{
		{"stdout", fmt.Sprintf("printf '%%s' %q", large)},
		{"stderr", fmt.Sprintf("printf '%%s' %q >&2", large)},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFetchFixture(t, test.script)
			started := time.Now()
			err := f.fetch(context.Background(), 61, "owner/repo", f.candidate, f.base, "operator-secret")
			if !errors.Is(err, errMaterialize) || time.Since(started) > 4*time.Second {
				t.Fatalf("oversized fetch result=%v duration=%s", err, time.Since(started))
			}
			if strings.Contains(err.Error(), f.root) || strings.Contains(err.Error(), "operator-secret") || strings.Contains(err.Error(), strings.Repeat("x", 32)) {
				t.Fatalf("unsafe failure: %v", err)
			}
		})
	}
}

func containsSequence(haystack, needle []string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func hasPrefix(value, prefix []string) bool {
	if len(value) < len(prefix) {
		return false
	}
	for i := range prefix {
		if value[i] != prefix[i] {
			return false
		}
	}
	return true
}
func suffixAfter(value []string, first string) []string {
	for i, item := range value {
		if item == first {
			return value[i:]
		}
	}
	return nil
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
