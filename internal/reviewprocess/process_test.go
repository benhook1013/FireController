package reviewprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

type processFixture struct {
	manager         *reviewworkspace.Manager
	workspace       reviewworkspace.Workspace
	dir             string
	base, candidate string
}

func newProcessFixture(t *testing.T) processFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executable scripts require a Unix shell")
	}
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	source := filepath.Join(root, "source")
	if err = os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = source
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("temporary git setup failed: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "--quiet")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.invalid")
	if err = os.WriteFile(filepath.Join(source, "source.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("commit", "--quiet", "-m", "base")
	base := runGit("rev-parse", "HEAD")
	if err = os.WriteFile(filepath.Join(source, "source.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("commit", "--quiet", "-m", "candidate")
	candidate := runGit("rev-parse", "HEAD")
	dataRoot := filepath.Join(root, "private-data")
	manager, err := reviewworkspace.New(dataRoot, git)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}
	cache, err := manager.CachePath(991)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "clone", "--quiet", "--bare", source, cache).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v %s", err, out)
	}
	if err := os.Chmod(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := manager.Prepare(context.Background(), 991, "attempt_1", candidate, base)
	if err != nil {
		t.Fatalf("workspace prepare: %v", err)
	}
	w, err = manager.BindRepository(context.Background(), w, "owner/repo")
	if err != nil {
		t.Fatalf("bind canonical repository: %v", err)
	}
	return processFixture{manager: manager, workspace: w, dir: root, base: base, candidate: candidate}
}

func (f processFixture) executable(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(f.dir, "fake-agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f processFixture) runner(t *testing.T, script string) *Runner {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "ambient-secret-token")
	t.Setenv("GH_TOKEN", "ambient-gh-secret")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(f.dir, "ambient-git-config"))
	privateHome := filepath.Join(f.dir, "private-home")
	if err := os.Mkdir(privateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := New(f.manager, f.executable(t, script), map[string]string{"HOME": privateHome})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f processFixture) runnerExecutable(t *testing.T, executable string) *Runner {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "ambient-secret-token")
	privateHome := filepath.Join(f.dir, "private-home")
	if err := os.Mkdir(privateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := New(f.manager, executable, map[string]string{"HOME": privateHome})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func usageJSON(repository, organization string, remaining, limit, full, rolling int64) string {
	return fmt.Sprintf(`{"type":"status","phase":"usage","status":"complete","organization":%q,"includedReviews":{"state":"available","repository":%q,"remaining":%d,"limit":%d,"fullCapacityInMs":%d,"rollingWindowMs":%d},"billingPeriod":{"state":"available","usageBillingStatus":"inactive"}}`, organization, repository, remaining, limit, full, rolling)
}

func TestUsageAndReviewUseExactCommandsAndClosedEnvironment(t *testing.T) {
	f := newProcessFixture(t)
	log := filepath.Join(f.dir, "invocations.log")
	envLog := filepath.Join(f.dir, "auth-env.log")
	script := fmt.Sprintf(`printf '%%s\n' "$PWD" "$@" >> %q
printf '%%s|%%s' "${GITHUB_TOKEN-unset}" "$HOME" >> %q
if [ "$1" = usage ]; then printf '%%s' %q; else printf 'raw review capture'; printf 'private diagnostic' >&2; exit 7; fi`, log, envLog, usageJSON("owner/repo", "owner", 4, 5, 12000, 3600000))
	r := f.runner(t, script)
	u, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.Remaining != 4 || u.Limit != 5 || u.FullCapacityInMS != 12000 || u.RollingWindowMS != 3600000 {
		t.Fatalf("usage facts: %+v", u)
	}
	capture, err := r.Review(context.Background(), f.workspace)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !capture.Started || capture.ExitCode != 7 || string(capture.Stdout) != "raw review capture" || string(capture.Stderr) != "private diagnostic" {
		t.Fatalf("review capture: %+v", capture)
	}
	lines, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := f.workspace.Directory + "\nusage\n--agent\n" + f.workspace.Directory + "\nreview\n--agent\n--committed\n--base\n" + f.workspace.ComparisonRef + "\n"
	if string(lines) != want {
		t.Fatalf("unexpected argv/cwd log: %q", lines)
	}
	env, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(env), "unset|") != 2 || !strings.Contains(string(env), "unset|"+filepath.Join(f.dir, "private-home")) || strings.Contains(string(env), "ambient-secret-token") || strings.Contains(string(env), "ambient-gh-secret") {
		t.Fatalf("child environment leaked or omitted configured auth: %q", env)
	}
	if _, err := r.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
		t.Fatalf("permit was reusable: %v", err)
	}
}

func TestUsageParserRejectsMissingAmbiguousAndUntrustedFacts(t *testing.T) {
	valid := usageJSON("owner/repo", "owner", 3, 5, 0, 86400000)
	if u, err := parseUsage([]byte(valid), "owner/repo", "owner"); err != nil || u.Remaining != 3 {
		t.Fatalf("valid usage: %+v %v", u, err)
	}
	bad := map[string]string{
		"wrong repository":      usageJSON("owner/other", "owner", 3, 5, 0, 1),
		"wrong organization":    usageJSON("owner/repo", "other", 3, 5, 0, 1),
		"remaining above limit": usageJSON("owner/repo", "owner", 6, 5, 0, 1),
		"negative limit":        usageJSON("owner/repo", "owner", 1, -1, 0, 1),
		"overflow":              strings.Replace(valid, `"remaining":3`, `"remaining":9223372036854775808`, 1),
		"null known field":      strings.Replace(valid, `"remaining":3`, `"remaining":null`, 1),
		"duplicate key":         strings.Replace(valid, `"remaining":3`, `"remaining":3,"remaining":3`, 1),
		"case alias":            strings.Replace(valid, `"phase":"usage"`, `"Phase":"usage","phase":"usage"`, 1),
		"incomplete object":     `{"type":"status","phase":"usage","status":"complete","organization":"owner","includedReviews":{"state":"available","repository":"owner/repo","remaining":1,"limit":2,"fullCapacityInMs":0},"billingPeriod":{"state":"available","usageBillingStatus":"inactive"}}`,
		"multiple events":       valid + valid,
		"array root":            "[" + valid + "]",
		"wrong state":           strings.Replace(valid, `"state":"available"`, `"state":"unknown"`, 1),
	}
	for name, input := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parseUsage([]byte(input), "owner/repo", "owner"); !errorCode(err, "usage_unavailable") && !errorCode(err, "rate_limited") {
				t.Fatalf("accepted untrusted usage: %v", err)
			}
		})
	}
	future := strings.Replace(valid, `"type":"status"`, `"futureField":{"note":"ignored"},"type":"status"`, 1)
	future = strings.Replace(future, `"rollingWindowMs":86400000`, `"rollingWindowMs":86400000,"billingPeriod":"not authorization"`, 1)
	future = strings.Replace(future, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":"inactive","scope":"user","user":"review-user","futureField":true`, 1)
	if _, err := parseUsage([]byte(future), "owner/repo", "owner"); err != nil {
		t.Fatalf("unrelated future fields were not ignored: %v", err)
	}
	for name, input := range map[string]string{"owner colon": "bad:org/repo", "extra path": "owner/repo/name", "whitespace": " owner/repo"} {
		t.Run(name, func(t *testing.T) {
			if validRepository(input) {
				t.Fatalf("accepted repository selector %q", input)
			}
		})
	}
}

func TestUsageParserRequiresAvailableInactiveBilling(t *testing.T) {
	valid := usageJSON("owner/repo", "owner", 1, 3, 0, 3600000)
	const safeBilling = `"billingPeriod":{"state":"available","usageBillingStatus":"inactive"}`
	bad := map[string]string{
		"missing":                  strings.Replace(valid, ","+safeBilling, "", 1),
		"null":                     strings.Replace(valid, safeBilling, `"billingPeriod":null`, 1),
		"string":                   strings.Replace(valid, safeBilling, `"billingPeriod":"inactive"`, 1),
		"array":                    strings.Replace(valid, safeBilling, `"billingPeriod":[]`, 1),
		"empty object":             strings.Replace(valid, safeBilling, `"billingPeriod":{}`, 1),
		"missing state":            strings.Replace(valid, safeBilling, `"billingPeriod":{"usageBillingStatus":"inactive"}`, 1),
		"missing status":           strings.Replace(valid, safeBilling, `"billingPeriod":{"state":"available"}`, 1),
		"null state":               strings.Replace(valid, safeBilling, `"billingPeriod":{"state":null,"usageBillingStatus":"inactive"}`, 1),
		"null status":              strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":null`, 1),
		"invalid state type":       strings.Replace(valid, safeBilling, `"billingPeriod":{"state":true,"usageBillingStatus":"inactive"}`, 1),
		"invalid status type":      strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":false`, 1),
		"unavailable":              strings.Replace(valid, safeBilling, `"billingPeriod":{"state":"unavailable","usageBillingStatus":"inactive"}`, 1),
		"unknown state":            strings.Replace(valid, safeBilling, `"billingPeriod":{"state":"unknown","usageBillingStatus":"inactive"}`, 1),
		"active":                   strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":"active"`, 1),
		"unknown status":           strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":"unknown"`, 1),
		"root alias only":          strings.Replace(valid, `"billingPeriod":`, `"BillingPeriod":`, 1),
		"root alias alongside":     strings.Replace(valid, safeBilling, safeBilling+`,"BillingPeriod":{}`, 1),
		"state alias only":         strings.Replace(valid, safeBilling, `"billingPeriod":{"State":"available","usageBillingStatus":"inactive"}`, 1),
		"state alias alongside":    strings.Replace(valid, safeBilling, `"billingPeriod":{"state":"available","State":"available","usageBillingStatus":"inactive"}`, 1),
		"status alias only":        strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"UsageBillingStatus":"inactive"`, 1),
		"status alias alongside":   strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":"inactive","UsageBillingStatus":"inactive"`, 1),
		"duplicate billing object": strings.Replace(valid, safeBilling, safeBilling+","+safeBilling, 1),
		"duplicate status":         strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":"active","usageBillingStatus":"inactive"`, 1),
	}
	for name, input := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parseUsage([]byte(input), "owner/repo", "owner"); !errorCode(err, "usage_unavailable") || outcomeUnknown(err) {
				t.Fatalf("unsafe billing facts were not refused before dispatch: %v", err)
			}
		})
	}
}

func TestUnsafeBillingRefreshClearsPermitWithoutReview(t *testing.T) {
	f := newProcessFixture(t)
	usageFile := filepath.Join(f.dir, "usage.json")
	count := filepath.Join(f.dir, "review.count")
	r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then cat %q; else echo review >> %q; fi`, usageFile, count))
	valid := usageJSON("owner/repo", "owner", 1, 3, 0, 3600000)
	for _, unsafe := range []string{
		strings.Replace(valid, `"usageBillingStatus":"inactive"`, `"usageBillingStatus":"active"`, 1),
		strings.Replace(valid, `,"billingPeriod":{"state":"available","usageBillingStatus":"inactive"}`, "", 1),
	} {
		if err := os.WriteFile(usageFile, []byte(valid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
			t.Fatalf("safe usage: %v", err)
		}
		if err := os.WriteFile(usageFile, []byte(unsafe), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); !errorCode(err, "usage_unavailable") {
			t.Fatalf("unsafe billing refresh: %v", err)
		}
		if _, err := r.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
			t.Fatalf("unsafe billing refresh left old permit: %v", err)
		}
		if _, err := os.Stat(count); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe billing refresh launched a review: %v", err)
		}
	}
	if err := os.WriteFile(usageFile, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatalf("restored safe usage: %v", err)
	}
	if _, err := r.Review(context.Background(), f.workspace); err != nil {
		t.Fatalf("restored safe permit: %v", err)
	}
	if data, err := os.ReadFile(count); err != nil || string(data) != "review\n" {
		t.Fatalf("safe recovery did not launch exactly one review: %q %v", data, err)
	}
}

func TestZeroIncludedUsageIsExplicitRateLimited(t *testing.T) {
	f := newProcessFixture(t)
	r := f.runner(t, fmt.Sprintf(`printf '%%s' %q`, usageJSON("owner/repo", "owner", 0, 5, 0, 1)))
	if got, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); !errorCode(err, "rate_limited") || got.Remaining != 0 || got.Limit != 5 || got.FullCapacityInMS != 0 || got.RollingWindowMS != 1 {
		t.Fatalf("zero allowance result: %v", err)
	}
	if _, err := r.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
		t.Fatalf("zero allowance authorized review: %v", err)
	}
}

func TestUsageMustAuthorizeOneExactFreshWorkspaceReview(t *testing.T) {
	f := newProcessFixture(t)
	count := filepath.Join(f.dir, "review.count")
	script := fmt.Sprintf(`if [ "$1" = usage ]; then echo usage >> %q; printf '%%s' %q; else echo review >> %q; printf ok; fi`, count, usageJSON("owner/repo", "owner", 1, 1, 1, 1), count)
	r := f.runner(t, script)
	if _, err := r.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
		t.Fatalf("review without usage ran: %v", err)
	}
	if _, err := os.Stat(count); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("review launched without permit: %v", err)
	}
	if _, err := r.Usage(context.Background(), f.workspace, "wrong/repo", "owner"); !errorCode(err, "usage_unavailable") {
		t.Fatalf("repository mismatch accepted: %v", err)
	}
	if _, err := os.Stat(count); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository mismatch dispatched CLI: %v", err)
	}
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	changed := f.workspace
	changed.ComparisonRef = "refs/heads/main"
	if _, err := r.Review(context.Background(), changed); !errorCode(err, "source_changed") {
		t.Fatalf("changed workspace accepted: %v", err)
	}
	if _, err := r.Review(context.Background(), f.workspace); err != nil {
		t.Fatalf("workspace-bound permit was not retained for its exact workspace: %v", err)
	}
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "wrong-owner"); !errorCode(err, "usage_unavailable") {
		t.Fatalf("wrong org usage: %v", err)
	}
	if _, err := r.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
		t.Fatalf("failed refresh left old permit: %v", err)
	}
	data, err := os.ReadFile(count)
	if err != nil || strings.Count(string(data), "review") != 1 {
		t.Fatalf("review launch count after exact-workspace permit: %q err=%v", data, err)
	}
}

func TestUsagePermitExpiresAndInvalidConstructorFieldsAreRejected(t *testing.T) {
	f := newProcessFixture(t)
	count := filepath.Join(f.dir, "review.count")
	r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else echo review >> %q; fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1), count))
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return clock }
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(permitLifetime + time.Nanosecond)
	if _, err := r.Review(context.Background(), f.workspace); !errorCode(err, "permit_required") {
		t.Fatalf("expired permit accepted: %v", err)
	}
	if _, err := os.Stat(count); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired permit launched a process: %v", err)
	}
	if _, err := New(f.manager, f.executable(t, "exit 0"), map[string]string{"GH_TOKEN": "secret"}); !errorCode(err, "configuration_invalid") {
		t.Fatalf("unapproved auth variable accepted: %v", err)
	}
	if _, err := New(f.manager, f.executable(t, "exit 0"), map[string]string{"HOME": "relative/home"}); !errorCode(err, "configuration_invalid") {
		t.Fatalf("relative auth path accepted: %v", err)
	}
}

func TestConcurrentReviewsConsumePermitExactlyOnce(t *testing.T) {
	f := newProcessFixture(t)
	count := filepath.Join(f.dir, "review.count")
	r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else echo started >> %q; sleep 0.2; printf complete; fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1), count))
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; _, errs[i] = r.Review(context.Background(), f.workspace) }(i)
	}
	close(start)
	wg.Wait()
	success, denied := 0, 0
	for _, err := range errs {
		if err == nil {
			success++
		} else if errorCode(err, "permit_required") {
			denied++
		} else {
			t.Errorf("unexpected review result: %v", err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("one-use permit results: success=%d denied=%d errors=%v", success, denied, errs)
	}
	data, err := os.ReadFile(count)
	if err != nil || strings.Count(string(data), "started") != 1 {
		t.Fatalf("review launches=%q err=%v", data, err)
	}
}

func TestReviewCancellationAndOutputBoundsAreUnknownAfterStart(t *testing.T) {
	f := newProcessFixture(t)
	started := filepath.Join(f.dir, "started")
	r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else touch %q; sleep 30; fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1), started))
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.Review(ctx, f.workspace); done <- err }()
	deadline := time.After(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("review process did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !outcomeUnknown(err) {
			t.Fatalf("canceled process not marked uncertain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled process did not return within bound")
	}

	f2 := newProcessFixture(t)
	big := strings.Repeat("x", maxReviewOutput+1)
	r2 := f2.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else printf '%%s' %q; fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1), big))
	if _, err := r2.Usage(context.Background(), f2.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	capture, err := r2.Review(context.Background(), f2.workspace)
	if !outcomeUnknown(err) || !capture.Started || len(capture.Stdout) > maxReviewOutput {
		t.Fatalf("output-bound result capture=%d started=%v err=%v", len(capture.Stdout), capture.Started, err)
	}
	if strings.Contains(err.Error(), f2.dir) || strings.Contains(err.Error(), "x") {
		t.Fatalf("unsafe error text: %v", err)
	}
}

func TestReviewOutputOverflowCancelsChildThatIgnoresClosedPipe(t *testing.T) {
	f := newProcessFixture(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for a child that deliberately ignores EPIPE")
	}
	marker := filepath.Join(f.dir, "started-once")
	status := usageJSON("owner/repo", "owner", 1, 1, 1, 1)
	script := fmt.Sprintf(`#!%s
import sys, time
if sys.argv[1] == "usage":
    print(%q)
    raise SystemExit(0)
with open(%q, "w", encoding="utf-8") as f:
    f.write("started")
try:
    sys.stdout.buffer.write(b"x" * %d)
    sys.stdout.flush()
except BrokenPipeError:
    time.sleep(30)
time.sleep(30)
`, python, status, marker, maxReviewOutput+1)
	executable := filepath.Join(f.dir, "fake-overflow-agent")
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := f.runnerExecutable(t, executable)
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatalf("usage: %v", err)
	}
	started := time.Now()
	capture, err := r.Review(context.Background(), f.workspace)
	duration := time.Since(started)
	if !outcomeUnknown(err) || !capture.Started || len(capture.Stdout) > maxReviewOutput || duration > 3*time.Second {
		t.Fatalf("overflow did not promptly cancel: started=%v bytes=%d duration=%s err=%v", capture.Started, len(capture.Stdout), duration, err)
	}
	if data, readErr := os.ReadFile(marker); readErr != nil || string(data) != "started" {
		t.Fatalf("child launch marker=%q err=%v", data, readErr)
	}
	if strings.Contains(err.Error(), f.dir) || strings.Contains(err.Error(), strings.Repeat("x", 32)) {
		t.Fatalf("unsafe overflow error: %v", err)
	}
}

func TestReviewSourceChangeRetainsCaptureAndDescendantPipeIsBounded(t *testing.T) {
	f := newProcessFixture(t)
	r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else printf 'captured'; printf 'tamper\n' > source.txt; exit 9; fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1)))
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	capture, err := r.Review(context.Background(), f.workspace)
	if !errorCode(err, "source_changed") || !outcomeUnknown(err) || !capture.Started || string(capture.Stdout) != "captured" {
		t.Fatalf("source mutation result capture=%+v err=%v", capture, err)
	}

	f2 := newProcessFixture(t)
	pidFile := filepath.Join(f2.dir, "descendant.pid")
	r2 := f2.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else sleep 30 & echo $! > %q; exit 0; fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1), pidFile))
	if _, err := r2.Usage(context.Background(), f2.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = r2.Review(context.Background(), f2.workspace)
	if !outcomeUnknown(err) || time.Since(started) > 4*time.Second {
		t.Fatalf("inherited pipe was not bounded: duration=%s err=%v", time.Since(started), err)
	}
	assertOwnedDescendantExited(t, readOwnedDescendantPID(t, pidFile))
}

func TestReviewStderrLimitIsApplied(t *testing.T) {
	f := newProcessFixture(t)
	tooMuch := strings.Repeat("e", maxErrorOutput+1)
	r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else printf '%%s' %q >&2; fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1), tooMuch))
	if _, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner"); err != nil {
		t.Fatal(err)
	}
	capture, err := r.Review(context.Background(), f.workspace)
	if !outcomeUnknown(err) || !capture.Started || len(capture.Stderr) > maxErrorOutput {
		t.Fatalf("stderr bound result length=%d started=%v err=%v", len(capture.Stderr), capture.Started, err)
	}
}

func errorCode(err error, code string) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Code == code
}
func outcomeUnknown(err error) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.OutcomeUnknown
}

func ownedDescendantRunning(pid int) bool {
	// A zombie has exited; only its system parent can reap it. Do not mistake
	// a defunct adopted worker for execution that survived cancellation.
	output, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.TrimSpace(string(output)) != "" && !strings.HasPrefix(strings.TrimSpace(string(output)), "Z")
}

func readOwnedDescendantPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err == nil && parseErr == nil && pid > 1 && pid != os.Getpid() {
			t.Cleanup(func() {
				if ownedDescendantRunning(pid) {
					// Safety cleanup on failure only, never the success condition.
					if process, err := os.FindProcess(pid); err == nil {
						_ = process.Kill()
					}
					t.Errorf("owned descendant %d survived its test", pid)
				}
			})
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("owned descendant did not record its PID")
	return 0
}

func assertOwnedDescendantExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ownedDescendantRunning(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("review descendant survived owned process-group cleanup")
}

func TestReviewOwnedProcessGroupStopsWorkersAcrossExitModes(t *testing.T) {
	if !processGroupsSupported {
		t.Skip("concrete provider execution requires Unix process groups")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps required to observe descendant exit")
	}
	for _, mode := range []string{"cancel", "timeout", "overflow", "wrapper-exit"} {
		t.Run(mode, func(t *testing.T) {
			f := newProcessFixture(t)
			// This sibling remains in the test's group, outside the owned review
			// group. Review cancellation must leave it running.
			unrelated := exec.Command("sleep", "30")
			if err := unrelated.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
			pidFile := filepath.Join(f.dir, "owned-worker.pid")
			// This worker inherits the owned group, but closes all capture pipes.
			// The wrapper's ordinary exit must still terminate it.
			body := fmt.Sprintf(`sleep 30 </dev/null >/dev/null 2>&1 &
echo $! > %q
`, pidFile)
			switch mode {
			case "wrapper-exit":
				body += "printf 'wrapper capture'; exit 0"
			case "overflow":
				body += `while :; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done`
			default:
				body += "wait"
			}
			r := f.runner(t, fmt.Sprintf(`if [ "$1" = usage ]; then printf '%%s' %q; else
%s
fi`, usageJSON("owner/repo", "owner", 1, 1, 1, 1), body))
			if _, err := r.Usage(t.Context(), f.workspace, "owner/repo", "owner"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if mode == "timeout" {
				ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
			}
			defer cancel()
			type outcome struct {
				capture Capture
				err     error
			}
			done := make(chan outcome, 1)
			go func() { capture, err := r.Review(ctx, f.workspace); done <- outcome{capture, err} }()
			pid := readOwnedDescendantPID(t, pidFile)
			if mode == "cancel" {
				cancel()
			}
			var result outcome
			select {
			case result = <-done:
			case <-time.After(4 * time.Second):
				cancel()
				t.Fatal("owned review group did not stop within bound")
			}
			if !result.capture.Started {
				t.Fatal("fixture never started")
			}
			if mode == "wrapper-exit" {
				if result.err != nil || string(result.capture.Stdout) != "wrapper capture" || result.capture.ExitCode != 0 {
					t.Fatal("ordinary wrapper result changed", result.err)
				}
			} else if !outcomeUnknown(result.err) {
				t.Fatal("interrupted review was falsely completed", result.err)
			}
			assertOwnedDescendantExited(t, pid)
			if !ownedDescendantRunning(unrelated.Process.Pid) {
				t.Fatal("review cleanup killed an unrelated sibling process")
			}
			if _, err := r.Review(t.Context(), f.workspace); !errorCode(err, "permit_required") {
				t.Fatal("interruption retained spend permit", err)
			}
		})
	}
}

func TestOwnedProcessConfigurationRejectsUnsupportedAndStartFailureIsSafe(t *testing.T) {
	if !processGroupsSupported {
		if _, err := New(&reviewworkspace.Manager{}, os.Args[0], nil); !errorCode(err, "configuration_invalid") {
			t.Fatal("unsupported host accepted provider execution", err)
		}
		return
	}
	command := exec.CommandContext(t.Context(), filepath.Join(t.TempDir(), "absent-executable"))
	cleanup, err := configureOwnedProcess(command)
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err == nil {
		t.Fatal("missing executable started")
	}
	if err = cleanup(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatal("start failure acquired a group", err)
	}
	if err = command.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatal("start failure cancellation targeted a process", err)
	}
	// No process has been allocated: the ownership checks must never signal
	// process group zero (the caller) on either cancellation or cleanup.
	invalid := exec.CommandContext(t.Context(), os.Args[0])
	invalid.Process = &os.Process{Pid: 0}
	cleanup, err = configureOwnedProcess(invalid)
	if !errorCode(err, "configuration_invalid") || cleanup != nil {
		t.Fatal("preexisting process accepted as a new owned group", err)
	}
}

func TestUsageFailureStagesPreserveSafeErrorsAndRevokePermit(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		stage        FailureStage
	}{
		{"not-started", "", FailureStageCommandNotStarted},
		{"nonzero", "printf 'private provider token' >&2; exit 7", FailureStageCommandFailed},
		{"overflow", "head -c 65537 /dev/zero", FailureStageCommandFailed},
		{"timeout", "exec sleep 60", FailureStageTimeout},
		{"malformed-response", "printf 'private invalid provider output'", FailureStageResponseUnproven},
		{"caller-deadline", "exec sleep 60", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProcessFixture(t)
			r := f.runner(t, fmt.Sprintf("printf '%%s' %q", usageJSON("owner/repo", "owner", 1, 3, 0, 3600000)))
			if _, err := r.Usage(t.Context(), f.workspace, "owner/repo", "owner"); err != nil {
				t.Fatal(err)
			}
			if tc.name == "not-started" {
				if err := os.Remove(r.executable); err != nil {
					t.Fatal(err)
				}
			} else {
				f.executable(t, tc.script)
			}
			ctx := t.Context()
			cancel := func() {}
			if tc.name == "caller-deadline" {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
			}
			defer cancel()
			started := time.Now()
			usage, err := r.Usage(ctx, f.workspace, "owner/repo", "owner")
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "usage_unavailable" || failure.FailureStage != tc.stage || failure.OutcomeUnknown || usage != (Usage{}) || err.Error() != "Included review allowance could not be verified." {
				t.Fatalf("unsafe or missing usage stage: code=%v stage=%v", errorCode(err, "usage_unavailable"), failure)
			}
			if tc.name == "timeout" && (time.Since(started) < usageTimeout || time.Since(started) > usageTimeout+5*time.Second) {
				t.Fatal("internal usage deadline changed")
			}
			if _, err := r.Review(t.Context(), f.workspace); !errorCode(err, "permit_required") {
				t.Fatal("failed usage retained permission", err)
			}
		})
	}
}

func TestUsagePrecommandContextKeepsUnclassifiedFailure(t *testing.T) {
	f := newProcessFixture(t)
	r := f.runner(t, "exit 99")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, current := range []context.Context{nil, ctx} {
		_, err := r.Usage(current, f.workspace, "owner/repo", "owner")
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "usage_unavailable" || failure.FailureStage != "" || failure.OutcomeUnknown {
			t.Fatal("context failure gained a command stage", err)
		}
	}
	for _, stage := range []FailureStage{FailureStageCommandNotStarted, FailureStageTimeout, FailureStageCommandFailed, FailureStageResponseUnproven, "private_path_or_token"} {
		if got := (&Error{Code: "usage_unavailable", FailureStage: stage}).Error(); got != "Included review allowance could not be verified." {
			t.Fatal("stage exposed error details")
		}
	}
}

func TestZeroUsageVerifiesSourceBeforeExposingFacts(t *testing.T) {
	f := newProcessFixture(t)
	r := f.runner(t, fmt.Sprintf("printf 'tamper' > source.txt; printf '%%s' %q", usageJSON("owner/repo", "owner", 0, 3, 1234, 3600000)))
	got, err := r.Usage(context.Background(), f.workspace, "owner/repo", "owner")
	if !errorCode(err, "source_changed") || got != (Usage{}) {
		t.Fatal("changed workspace exposed trusted exhausted facts", got, err)
	}
	r.mu.Lock()
	permit := r.permit
	r.mu.Unlock()
	if permit != nil {
		t.Fatal("zero observation issued permit")
	}
}
