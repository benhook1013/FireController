package reviewworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type realFetchFixture struct {
	manager                         *Manager
	source, cache, log, git, python string
	base, candidate                 string
}

func fetchFixtureGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("real Git unavailable")
	}
	command := exec.Command(git, append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	command.Dir = directory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	body, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("private fixture Git failed: %v %s", err, body)
	}
	return strings.TrimSpace(string(body))
}

func realFetch(t *testing.T, rejectNetwork bool, provisionLocal bool) realFetchFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("controlled local Git requires Unix")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git unavailable")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	fetchFixtureGit(t, dir, "init", "--quiet", "--template=", source)
	path := filepath.Join(source, "source.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetchFixtureGit(t, source, "add", "source.txt")
	fetchFixtureGit(t, source, "commit", "--quiet", "-m", "base")
	base := fetchFixtureGit(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetchFixtureGit(t, source, "commit", "--quiet", "-am", "candidate")
	candidate := fetchFixtureGit(t, source, "rev-parse", "HEAD")
	root, log := filepath.Join(dir, "owned"), filepath.Join(dir, "calls.jsonl")
	wrapper := filepath.Join(dir, "controlled-git")
	script := fmt.Sprintf(`#!%s
import json,os,subprocess,sys
a=sys.argv[1:]
record={'args':a}
if 'fetch' in a:
 gitdir=next(x for x in a if x.startswith('--git-dir='))
 record['prior_refs']=subprocess.check_output([%q,gitdir,'for-each-ref','--format=%%(refname):%%(objectname)'],text=True).splitlines()
with open(%q,'a') as out:out.write(json.dumps(record)+'\n')
if 'fetch' in a:
 if %s:sys.exit(93)
 i=a.index('fetch')
 if a[i+1:i+6]!=['--no-tags','--no-recurse-submodules','--no-write-fetch-head','--','https://github.com/owner/repo.git']:sys.exit(94)
 a[i+5]=%q
 a=['-c','protocol.file.allow=always']+a
os.execv(%q,[%q]+a)
`, python, git, log, map[bool]string{true: "True", false: "False"}[rejectNetwork], source, git, git)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := New(root, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := manager.CachePath(77)
	fetchFixtureGit(t, dir, "init", "--quiet", "--bare", "--template=", cache)
	if err := os.Chmod(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	fetchFixtureGit(t, dir, "--git-dir="+cache, "config", "fetch.unpackLimit", "1")
	if provisionLocal {
		if err := os.CopyFS(filepath.Join(cache, "objects"), os.DirFS(filepath.Join(source, ".git", "objects"))); err != nil {
			t.Fatal(err)
		}
	}
	return realFetchFixture{manager: manager, source: source, cache: cache, log: log, git: git, python: python, base: base, candidate: candidate}
}

func (f realFetchFixture) fetchCalls(t *testing.T) []struct {
	Args      []string `json:"args"`
	PriorRefs []string `json:"prior_refs"`
} {
	t.Helper()
	body, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	var calls []struct {
		Args      []string `json:"args"`
		PriorRefs []string `json:"prior_refs"`
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var record struct {
			Args      []string `json:"args"`
			PriorRefs []string `json:"prior_refs"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if suffixAfter(record.Args, "fetch") != nil {
			calls = append(calls, record)
		}
	}
	return calls
}

func TestFetchCachedCompleteClosureSkipsNetworkAndPinsExactObjects(t *testing.T) {
	f := realFetch(t, true, true)
	if err := f.manager.Fetch(context.Background(), 77, "owner/repo", f.candidate, f.base, "private-fixture-token"); err != nil {
		t.Fatal(err)
	}
	if len(f.fetchCalls(t)) != 0 {
		t.Fatal("complete cache attempted network")
	}
	for ref, sha := range map[string]string{"candidate": f.candidate, "base": f.base} {
		if got := fetchFixtureGit(t, f.source, "--git-dir="+f.cache, "rev-parse", "refs/firecontroller/cache/"+ref); got != sha {
			t.Fatal("private pin differs from selected source")
		}
	}
	config, err := os.ReadFile(filepath.Join(f.cache, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "private-fixture-token") || strings.Contains(string(config), "Authorization") || strings.Contains(string(config), "github.com") {
		t.Fatal("credentials or remote were persisted")
	}
}

func TestQuietClosureDetectsMissingReachableBlobAndFetchFailsClosed(t *testing.T) {
	f := realFetch(t, true, true)
	if !f.manager.completeFetchObjects(context.Background(), f.cache, "--git-dir="+f.cache, f.candidate, f.base) {
		t.Fatal("complete fixture was unavailable")
	}
	blob := fetchFixtureGit(t, f.source, "rev-parse", "HEAD:source.txt")
	if err := os.Remove(filepath.Join(f.cache, "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if got := fetchFixtureGit(t, f.source, "--git-dir="+f.cache, "rev-parse", f.candidate+"^{commit}"); got != f.candidate {
		t.Fatal("fixture lost commit rather than blob")
	}
	if f.manager.completeFetchObjects(context.Background(), f.cache, "--git-dir="+f.cache, f.candidate, f.base) {
		t.Fatal("quiet traversal missed absent blob")
	}
	if err := f.manager.Fetch(context.Background(), 77, "owner/repo", f.candidate, f.base, ""); !errors.Is(err, errMaterialize) {
		t.Fatalf("missing blob accepted: %v", err)
	}
	if len(f.fetchCalls(t)) != 1 {
		t.Fatal("incomplete cache did not attempt bounded source fetch")
	}
	if got := fetchFixtureGit(t, f.source, "--git-dir="+f.cache, "for-each-ref", "--format=%(refname)"); got != "" {
		t.Fatal("unverified objects gained cache pins")
	}
}

func TestFetchNewTipNegotiatesExistingPrivatePinsWithoutDuplicatingHistory(t *testing.T) {
	f := realFetch(t, false, false)
	if err := f.manager.Fetch(context.Background(), 77, "owner/repo", f.candidate, f.base, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.source, "source.txt"), []byte("new tip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetchFixtureGit(t, f.source, "commit", "--quiet", "-am", "new tip")
	next := fetchFixtureGit(t, f.source, "rev-parse", "HEAD")
	if err := f.manager.Fetch(context.Background(), 77, "owner/repo", next, f.base, ""); err != nil {
		t.Fatal(err)
	}
	calls := f.fetchCalls(t)
	if len(calls) != 2 {
		t.Fatalf("want one cold and one incremental fetch, got %d", len(calls))
	}
	for _, want := range []string{"refs/firecontroller/cache/candidate:" + f.candidate, "refs/firecontroller/cache/base:" + f.base} {
		if !strings.Contains(strings.Join(calls[1].PriorRefs, "\n"), want) {
			t.Fatal("new fetch had no retained exact negotiation pin")
		}
	}
	counts := fetchFixtureGit(t, f.source, "--git-dir="+f.cache, "count-objects", "-v")
	for _, line := range strings.Split(counts, "\n") {
		if strings.HasPrefix(line, "in-pack: ") {
			n, _ := strconv.Atoi(strings.TrimPrefix(line, "in-pack: "))
			if n != 9 {
				t.Fatalf("three one-file commits should retain nine distinct packed objects, got %d", n)
			}
		}
	}
	if err := f.manager.Fetch(context.Background(), 77, "owner/repo", next, f.base, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.fetchCalls(t)) != 2 {
		t.Fatal("same verified tip fetched again")
	}
}

func TestFetchCancellationTerminatesOwnedBackgroundDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process groups required")
	}
	for _, deadline := range []bool{true, false} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "descendant-survived")
			behavior := fmt.Sprintf("sh -c 'sleep 0.5; echo survived > %s' </dev/null >/dev/null 2>&1 & :", strconv.Quote(marker))
			if deadline {
				behavior += "; sleep 30"
			}
			f := newFetchFixture(t, behavior)
			ctx := context.Background()
			cancel := func() {}
			if deadline {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
			}
			defer cancel()
			err := f.fetch(ctx, 88, "owner/repo", f.candidate, f.base, "")
			if deadline {
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errMaterialize) {
					t.Fatalf("deadline not retained: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			time.Sleep(650 * time.Millisecond)
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned descendant survived Git command: %v", err)
			}
		})
	}
}
