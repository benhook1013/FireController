package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

type cliDriverReader struct {
	mu              sync.Mutex
	repository      reviewprovider.Repository
	pulls           map[int64]reviewprovider.Pull
	calls           map[int64]int
	repositoryCalls int
}

func (r *cliDriverReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repositoryCalls++
	if err := ctx.Err(); err != nil {
		return reviewprovider.Repository{}, err
	}
	return r.repository, nil
}
func (r *cliDriverReader) Pull(ctx context.Context, _ string, number int64) (reviewprovider.Pull, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[number]++
	if err := ctx.Err(); err != nil {
		return reviewprovider.Pull{}, err
	}
	p, ok := r.pulls[number]
	if !ok {
		return p, errors.New("synthetic missing scoped PR")
	}
	return p, nil
}

type cliDriverFixture struct {
	*cliEvidenceFixture
	driver     *CLIDriver
	reader     *cliDriverReader
	runner     *reviewprocess.Runner
	capability OperatorCapability
	log        string
	fetches    atomic.Int64
}

func cliShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func newCLIDriverFixture(t *testing.T) *cliDriverFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("local fake executable uses a POSIX shell; no Windows runtime claim")
	}
	f := &cliDriverFixture{cliEvidenceFixture: newCLIEvidenceFixture(t)}
	for _, ensure := range []func(context.Context, *core.Store) error{reviewexec.EnsureAdjudicationSchema, reviews.EnsureSchema} {
		if err := ensure(context.Background(), f.store); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	f.log = filepath.Join(root, "argv.log")
	executable := filepath.Join(root, "synthetic-cli")
	// This local executable exercises the real Runner's fixed allowance/review
	// argv and parser. It has no provider or network behavior.
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + cliShellQuote(f.log) + `
if [ "$1" = usage ]; then
 printf '%s\n' '{"type":"status","phase":"usage","status":"complete","organization":"fixture-org","includedReviews":{"state":"available","repository":"owner/repo","remaining":2,"limit":3,"fullCapacityInMs":0,"rollingWindowMs":60000},"billingPeriod":{"state":"available","usageBillingStatus":"inactive"}}'
 exit 0
fi
printf '{"type":"review_context","reviewType":"committed","currentBranch":"HEAD","baseBranch":"%s","workingDirectory":"%s"}\n' "$5" "$PWD"
printf '%s\n' '{"type":"finding","title":"Synthetic finding","comment":"Plain retained detail","severity":"major","fileName":"reviewed.txt","startLine":1,"endLine":1}'
printf '%s\n' '{"type":"complete","status":"review_completed","outcome":"completed","findings":1,"reviewedFiles":["reviewed.txt","second.txt"]}'
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f.capability = OperatorCapability{ID: "runtime-capability", Account: "runtime-account", Organization: "fixture-org", Channels: []string{"cli", "hosted"}, GitExecutable: git, CLIExecutable: executable, AuthenticationEnvironment: map[string]string{}, githubCredential: "synthetic-operator-token"}
	f.capability.Fingerprint, err = capabilityFingerprint(f.capability)
	if err != nil {
		t.Fatal(err)
	}
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	f.runner, err = reviewprocess.New(f.manager, executable, f.capability.AuthenticationEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	f.reader = &cliDriverReader{repository: reviewprovider.Repository{ID: 901, FullName: "owner/repo", DefaultBranch: "main"}, pulls: map[int64]reviewprovider.Pull{}, calls: map[int64]int{}}
	for number := int64(2); number <= 9; number++ {
		f.reader.pulls[number] = reviewprovider.Pull{Number: number, RepositoryID: 901, Title: fmt.Sprintf("Fresh synthetic PR %d", number), State: "open", HeadSHA: f.headSHA, BaseSHA: f.baseSHA, BaseRef: "main", ChangedFiles: 999}
	}
	f.driver, err = NewCLIDriver(f.store, f.engine, f.capability, f.manager, f.captures, f.reader, f.runner)
	if err != nil {
		t.Fatal(err)
	}
	f.driver.fetch = func(ctx context.Context, id int64, repo, candidate, base, token string) error {
		f.fetches.Add(1)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if id != 901 || repo != "owner/repo" || token != f.capability.GitHubCredential() {
			return errors.New("incorrect synthetic fetch binding")
		}
		// Objects were provisioned into this real private cache by the fixture's
		// local Git push. Never contact GitHub from a driver test.
		return nil
	}
	return f
}

func (f *cliDriverFixture) start(t *testing.T, number, revision int64, key string) (reviewexec.Operation, reviewexec.Attempt, reviewexec.Configuration) {
	t.Helper()
	request := reviewexec.Request{ExpectedConfigurationRevision: revision, PullNumber: number, Channel: "cli", HeadSHA: f.reader.pulls[number].HeadSHA}
	body, err := f.engine.Enqueue(context.Background(), f.scope(t, f.writer, false), key, request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	a, err := f.engine.StartAttempt(context.Background(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.engine.ExecutionContext(context.Background(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return current.Operation, a, current.Configuration
}
func (f *cliDriverFixture) prepareAdmit(t *testing.T, number, revision int64, key string) (reviewexec.Operation, reviewexec.Attempt) {
	t.Helper()
	op, a, c := f.start(t, number, revision, key)
	p, err := f.driver.Prepare(context.Background(), op, c, a)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.engine.Admit(context.Background(), f.project, a.ID, a.Revision, p.Anchor, p.Intent)
	if err != nil {
		t.Fatal(err)
	}
	return op, a
}
func (f *cliDriverFixture) configure(t *testing.T, mode string, queue []int64) {
	t.Helper()
	previous := int64(1)
	if _, err := f.engine.Configure(context.Background(), f.scope(t, f.admin, true), "driver-configuration", reviewexec.Configuration{ExpectedRevision: &previous, CapabilityID: f.capability.ID, Mode: mode, Queue: queue, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIDriverRealRunnerCompletesOneIncludedReviewAndRetainsEvidence(t *testing.T) {
	f := newCLIDriverFixture(t)
	op, a := f.prepareAdmit(t, 2, 1, "native-driver-one")
	result, err := f.driver.Dispatch(context.Background(), op, a, func(context.Context, string) error { t.Fatal("CLI used hosted trigger callback"); return nil })
	if err != nil || result.State != "completed" {
		t.Fatal("complete retained CLI did not settle", result, err)
	}
	if _, err := f.engine.Advance(context.Background(), f.project, a.ID, a.Revision, result.State, result.Reason, ""); err != nil {
		t.Fatal(err)
	}
	lines, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(lines)), "\n")
	if len(args) != 2 || args[0] != "usage --agent" || args[1] != "review --agent --committed --base refs/firecontroller/review-base/"+a.ID || strings.Contains(string(lines), "--use-credits") || strings.Contains(string(lines), f.capability.GitHubCredential()) {
		t.Fatal("runner repeated work or changed included-only argv", string(lines))
	}
	if f.reader.repositoryCalls != 2 || f.reader.calls[2] != 2 || len(f.reader.calls) != 1 || f.fetches.Load() != 1 {
		t.Fatal("phase reads were not scoped/memoized", f.reader.calls, f.reader.repositoryCalls)
	}
	var id string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	evidence, err := f.engine.ReadCLIEvidence(context.Background(), f.scopeWithAccess(t, f.readerToken(), false, "read"), id)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.engine.ReadCLIAdjudication(context.Background(), f.scopeWithAccess(t, f.readerToken(), false, "read"), id)
	if err != nil || !evidence.Capture.Review.Complete || evidence.Anchor != *a.Anchor || state.Finalized || state.Findings[0].Disposition != "unadjudicated" {
		t.Fatal("driver invented adjudication or lost source", err)
	}
	var nativeCount int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_runs r JOIN review_workspace w ON w.project_id=r.project_id AND w.snapshot_id=r.snapshot_id WHERE r.source='native-executor' AND r.source_run_id=?`, id).Scan(&nativeCount); err != nil || nativeCount != 1 {
		t.Fatal("capture missing browser projection", err)
	}
	repeated, err := f.driver.Dispatch(context.Background(), op, a, nil)
	if err != nil || repeated.State != "failed" {
		t.Fatal("duplicate Dispatch was not refused", repeated, err)
	}
	after, _ := os.ReadFile(f.log)
	if string(after) != string(lines) {
		t.Fatal("duplicate dispatch spent another process")
	}
}

func TestCLIDriverConstructorRejectsMisboundCapabilityAndCopiesInputs(t *testing.T) {
	f := newCLIDriverFixture(t)
	for _, mode := range []string{"nil-store", "nil-engine", "nil-manager", "nil-captures", "nil-reader", "typed-nil-reader", "nil-runner", "fingerprint", "account", "no-cli", "wrong-engine"} {
		t.Run(mode, func(t *testing.T) {
			store, engine, manager, captures, runner := f.store, f.engine, f.manager, f.captures, f.runner
			var reader reviewexec.TargetReader = f.reader
			capability := f.capability
			switch mode {
			case "nil-store":
				store = nil
			case "nil-engine":
				engine = nil
			case "nil-manager":
				manager = nil
			case "nil-captures":
				captures = nil
			case "nil-reader":
				reader = nil
			case "typed-nil-reader":
				reader = (*cliDriverReader)(nil)
			case "nil-runner":
				runner = nil
			case "fingerprint":
				capability.Fingerprint = strings.Repeat("f", 64)
			case "account":
				capability.Account = "different-account"
			case "no-cli":
				capability.Channels = []string{"hosted"}
			case "wrong-engine":
				var err error
				engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: capability.ID, Provider: "coderabbit", Account: capability.Account, Channels: capability.Channels, Fingerprint: strings.Repeat("f", 64)}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := NewCLIDriver(store, engine, capability, manager, captures, reader, runner); err == nil {
				t.Fatal("misbound concrete resources accepted")
			}
		})
	}
	f.capability.Channels[0] = "invalid-caller-change"
	f.capability.AuthenticationEnvironment["HOME"] = "invalid-caller-change"
	op, a, c := f.start(t, 2, 1, "copied-driver-capability")
	if _, err := f.driver.Prepare(context.Background(), op, c, a); err != nil {
		t.Fatal("caller-owned capability mutation changed driver", err)
	}
}

func TestCLIDriverInitialProjectionPrefersCurrentFactsAndDoesNotHideConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			f := newCLIDriverFixture(t)
			op, a, c := f.start(t, 2, 1, "projection-driver")
			metadata := reviews.PullInput{Number: 2, Title: "More current stored title", State: "open", HeadSHA: strings.Repeat("9", 40), BaseRef: "stored-base", BaseSHA: strings.Repeat("8", 40), CIState: "success", Mergeability: "mergeable", Complete: false}
			if _, err := f.engine.PublishAttempt(context.Background(), f.project, a.ID, &metadata); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := f.store.DB().Exec(`UPDATE review_attempts SET head_sha=? WHERE project_id=?`, strings.Repeat("f", 40), f.project); err != nil {
					t.Fatal(err)
				}
			}
			var beforeTitle, beforeHead string
			if err := f.store.DB().QueryRow(`SELECT p.title,p.head_sha FROM review_pulls p JOIN review_workspace w ON w.project_id=p.project_id AND w.snapshot_id=p.snapshot_id WHERE p.project_id=? AND p.number=2`, f.project).Scan(&beforeTitle, &beforeHead); err != nil {
				t.Fatal(err)
			}
			_, err := f.driver.Prepare(context.Background(), op, c, a)
			if conflict {
				assertCoreErrorCode(t, err, "observation_conflict")
			} else if err != nil {
				t.Fatal(err)
			}
			var title, head string
			if err := f.store.DB().QueryRow(`SELECT p.title,p.head_sha FROM review_pulls p JOIN review_workspace w ON w.project_id=p.project_id AND w.snapshot_id=p.snapshot_id WHERE p.project_id=? AND p.number=2`, f.project).Scan(&title, &head); err != nil || title != beforeTitle || head != beforeHead {
				t.Fatal("driver overwrote newer stored facts or hid a projection conflict", err)
			}
		})
	}
}

// The embedded fixture's reader token and this fixture's provider reader have
// intentionally distinct identities.
func (f *cliDriverFixture) readerToken() string { return f.cliEvidenceFixture.reader }

func TestCLIDriverFinalGuardsPreventProcessStart(t *testing.T) {
	for _, mode := range []string{"source-head", "policy", "revocation", "workspace", "expired-context"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			op, a := f.prepareAdmit(t, 2, 1, "guarded-driver")
			var runs atomic.Int64
			f.driver.review = func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error) {
				runs.Add(1)
				return reviewprocess.Capture{}, nil
			}
			switch mode {
			case "source-head":
				f.reader.pulls[2] = func() reviewprovider.Pull { p := f.reader.pulls[2]; p.HeadSHA = strings.Repeat("f", 40); return p }()
			case "policy":
				previous := int64(1)
				if _, err := f.engine.ConfigurePolicy(context.Background(), f.scope(t, f.admin, true), "changed-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &previous, RequiredChannels: []string{"cli"}, PausedChannels: []string{}, MaxReviewFiles: 1}); err != nil {
					t.Fatal(err)
				}
			case "revocation":
				if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='runtime-evidence-writer'`); err != nil {
					t.Fatal(err)
				}
			case "workspace":
				f.driver.mu.Lock()
				workspace := f.driver.prepared[a.ID].workspace
				f.driver.mu.Unlock()
				if err := os.WriteFile(filepath.Join(workspace.Directory, "reviewed.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "expired-context":
				f.driver.mu.Lock()
				f.driver.prepared[a.ID].expires = time.Now().Add(-time.Second)
				f.driver.mu.Unlock()
			}
			result, err := f.driver.Dispatch(context.Background(), op, a, nil)
			if err != nil || result.State != "failed" || runs.Load() != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("final guard allowed an external action", result, err)
			}
			if _, ok := f.driver.prepared[a.ID]; ok {
				t.Fatal("failed recheck retained reusable context")
			}
		})
	}
}

func TestCLIDriverGitFileCeilingAndPreparationBinding(t *testing.T) {
	for _, mode := range []string{"zero-unlimited", "two-files", "one-file-limit", "wrong-config", "wrong-operation"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			if mode == "one-file-limit" || mode == "two-files" {
				limit := int64(1)
				if mode == "two-files" {
					limit = 2
				}
				if _, err := f.store.DB().Exec(`UPDATE review_policy_config SET body_json=? WHERE project_id=?`, fmt.Sprintf(`{"required_channels":["cli"],"paused_channels":[],"max_review_files":%d}`, limit), f.project); err != nil {
					t.Fatal(err)
				}
			}
			op, a, c := f.start(t, 2, 1, "file-bound-driver")
			if mode == "wrong-config" {
				c.Mode = "stacked"
			}
			if mode == "wrong-operation" {
				op.HeadSHA = strings.Repeat("f", 40)
			}
			_, err := f.driver.Prepare(context.Background(), op, c, a)
			if mode == "zero-unlimited" || mode == "two-files" {
				if err != nil {
					t.Fatal("Git's two files were incorrectly compared to provider's999", err)
				}
			} else if err == nil {
				t.Fatal("unsafe preparation accepted")
			}
			if mode == "one-file-limit" {
				if _, err := os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("allowance subprocess ran before project size check")
				}
			}
		})
	}
}

func TestCLIDriverStackChecksFreshScopedPredecessors(t *testing.T) {
	for _, mode := range []string{"independent", "merged-prefix", "earlier-uncovered", "closed-prefix"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			configurationMode := "stacked"
			if mode == "independent" {
				configurationMode = "independent"
			}
			f.configure(t, configurationMode, []int64{2, 3, 4})
			if mode == "merged-prefix" {
				for _, number := range []int64{2, 3} {
					p := f.reader.pulls[number]
					p.Merged = true
					p.State = "closed"
					f.reader.pulls[number] = p
				}
			}
			if mode == "earlier-uncovered" {
				p := f.reader.pulls[4]
				p.BaseSHA = f.headSHA
				p.BaseRef = "feature-parent"
				f.reader.pulls[4] = p
			}
			if mode == "closed-prefix" {
				p := f.reader.pulls[3]
				p.State = "closed"
				f.reader.pulls[3] = p
			}
			op, a, c := f.start(t, 4, 2, "stack-driver")
			_, err := f.driver.Prepare(context.Background(), op, c, a)
			if mode == "independent" || mode == "merged-prefix" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unfinished stack advanced")
			}
			if f.reader.repositoryCalls != 1 {
				t.Fatal("repository was fetched repeatedly within phase")
			}
			for _, count := range f.reader.calls {
				if count != 1 {
					t.Fatal("PR reads were repeated within phase", f.reader.calls)
				}
			}
			if mode == "independent" && len(f.reader.calls) != 1 {
				t.Fatal("independent review inspected predecessors", f.reader.calls)
			}
			if mode == "earlier-uncovered" && f.reader.calls[2] != 1 {
				t.Fatal("driver checked only nearest parent, not earlier prefix", f.reader.calls)
			}
		})
	}
}

func seedCLIDriverNativeCoverage(t *testing.T, f *cliDriverFixture, number int64) {
	t.Helper()
	a, w := f.startAdmitted(t, number)
	contextOnly := makeIncompleteCLICapture(t, w)
	terminal, err := json.Marshal(map[string]any{"type": "complete", "status": "review_completed", "outcome": "completed", "findings": 0, "reviewedFiles": mustChangedFiles(t, f.manager, w)})
	if err != nil {
		t.Fatal(err)
	}
	contextOnly.Stdout = append(contextOnly.Stdout, append(terminal, '\n')...)
	result, err := RetainCLI(context.Background(), f.captures, f.engine, f.manager, a, w, contextOnly, time.Now().Add(-time.Second).UTC(), time.Now().UTC())
	if err != nil || !result.Complete {
		t.Fatal("synthetic complete source seed failed", err)
	}
	if _, err := f.engine.Advance(context.Background(), f.project, a.ID, a.Revision, "completed", "fixture_capture_retained", ""); err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	if _, err := f.engine.FinalizeCLI(context.Background(), f.scope(t, f.writer, false), "fixture-source-finalized", result.Receipt.ID, reviewexec.CLIFinalizeRequest{ExpectedRevision: &revision}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIDriverNativeCoverageHoldsTargetButImportCannot(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			f := newCLIDriverFixture(t)
			if native {
				seedCLIDriverNativeCoverage(t, f, 2)
			} else {
				zero := int64(0)
				run := reviews.RunInput{SourceRunID: "imported-completed-cli", Channel: "cli", HeadSHA: f.headSHA, Reviewer: "Synthetic imported reviewer", Scope: "synthetic import", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z"}
				input := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 901, Source: "synthetic-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{{Number: 2, Title: "Imported PR metadata", State: "open", HeadSHA: f.headSHA, BaseRef: "main", BaseSHA: f.baseSHA, CIState: "unknown", Mergeability: "unknown", Complete: true, Runs: []reviews.RunInput{run}}}}
				body, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				reviews.Register(mux, f.store)
				request := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/reviews/observations", strings.NewReader(string(body)))
				request.Header.Set("Authorization", "Bearer "+f.admin)
				request.Header.Set("Idempotency-Key", "imported-cli-not-native-credit")
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request)
				if response.Code != 201 {
					t.Fatalf("fixture import: %d %s", response.Code, response.Body.String())
				}
			}
			op, a, c := f.start(t, 2, 1, "after-existing-source")
			_, err := f.driver.Prepare(context.Background(), op, c, a)
			if native {
				assertCoreErrorCode(t, err, "review_held")
				if f.fetches.Load() != 0 {
					t.Fatal("already covered target did expensive workspace work")
				}
			} else if err != nil {
				t.Fatal("imported projection incorrectly authorized or blocked native coverage", err)
			}
		})
	}
}

func TestCLIDriverStackAcceptsActualNativePredecessorCoverage(t *testing.T) {
	f := newCLIDriverFixture(t)
	seedCLIDriverNativeCoverage(t, f, 2)
	// Add a genuine third commit only to the disposable source/cache so the
	// selected target has a nonempty comparison above its covered predecessor.
	source := filepath.Join(filepath.Dir(f.storeDir), "fixture-source")
	if err := os.WriteFile(filepath.Join(source, "third.txt"), []byte("third candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, source, "add", "third.txt")
	gitTestRun(t, source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "third")
	third := gitTestOutput(t, source, "rev-parse", "HEAD")
	gitTestRun(t, source, "push", "cache", "HEAD:refs/heads/third")
	p := f.reader.pulls[3]
	p.Merged = true
	p.State = "closed"
	f.reader.pulls[3] = p
	p = f.reader.pulls[4]
	p.HeadSHA, p.BaseSHA, p.BaseRef = third, f.headSHA, "covered-parent"
	f.reader.pulls[4] = p
	f.configure(t, "stacked", []int64{2, 3, 4})
	op, a, c := f.start(t, 4, 2, "covered-stack-target")
	prepared, err := f.driver.Prepare(context.Background(), op, c, a)
	if err != nil {
		t.Fatal("native-covered predecessor blocked target", err)
	}
	if prepared.Anchor.ParentPullNumber != 2 || prepared.Anchor.ParentSHA != f.headSHA || prepared.Anchor.CandidateSHA != third || f.reader.repositoryCalls != 1 || f.reader.calls[2] != 1 || f.reader.calls[3] != 1 || f.reader.calls[4] != 1 {
		t.Fatal("stack selected wrong source or repeated provider observations", prepared.Anchor, f.reader.calls)
	}
}

func TestCLIDriverCancelledAndPartialExecutionRetainsHonestEvidence(t *testing.T) {
	for _, mode := range []string{"partial", "cancelled", "no-start", "rate-limit"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			op, a := f.prepareAdmit(t, 2, 1, "retained-driver")
			var runs atomic.Int64
			entered := make(chan struct{})
			f.driver.review = func(ctx context.Context, w reviewworkspace.Workspace) (reviewprocess.Capture, error) {
				runs.Add(1)
				close(entered)
				if mode == "no-start" {
					return reviewprocess.Capture{ExitCode: -1}, &reviewprocess.Error{Code: "process_failed"}
				}
				capture := makeIncompleteCLICapture(t, w)
				if mode == "cancelled" {
					<-ctx.Done()
					return capture, &reviewprocess.Error{Code: "process_failed", OutcomeUnknown: true}
				}
				if mode == "rate-limit" {
					capture.Stdout = append(capture.Stdout, []byte("{\"type\":\"rate_limit\",\"outcome\":\"rate_limited\"}\n")...)
				}
				return capture, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type outcome struct {
				result reviewexec.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() { r, err := f.driver.Dispatch(ctx, op, a, nil); done <- outcome{r, err} }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("review seam not reached")
			}
			if mode == "cancelled" {
				cancel()
			}
			var got outcome
			select {
			case got = <-done:
			case <-time.After(7 * time.Second):
				t.Fatal("detached local retention exceeded bound")
			}
			want := "uncertain"
			if mode == "no-start" {
				want = "failed"
			}
			if mode == "rate-limit" {
				want = "rate_limited"
			}
			if got.err != nil || got.result.State != want || runs.Load() != 1 {
				t.Fatal("execution outcome was overclaimed", mode, got)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, a.ID) != 1 {
				t.Fatal("cancellation/partial capture was lost")
			}
			record, _, err := f.captures.Read(context.Background(), a.ID)
			if err != nil || record.StartedAt.IsZero() || record.FinishedAt.Before(record.StartedAt) {
				t.Fatal("measured private capture window missing", err)
			}
			var complete int
			if err := f.store.DB().QueryRow(`SELECT complete FROM review_runs r JOIN review_workspace w ON w.project_id=r.project_id AND w.snapshot_id=r.snapshot_id WHERE r.source='native-executor' AND r.channel='cli'`).Scan(&complete); err != nil || complete != 0 {
				t.Fatal("incomplete result created completed browser coverage", err)
			}
		})
	}
}

func cliDriverCompletedFinding(t *testing.T, f *cliDriverFixture) (reviewexec.Attempt, string) {
	t.Helper()
	op, a := f.prepareAdmit(t, 2, 1, "initial-real-capture")
	result, err := f.driver.Dispatch(t.Context(), op, a, nil)
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
	if _, err := f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, result.State, result.Reason, ""); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return a, id
}

func cliDriverDecision(t *testing.T, f *cliDriverFixture, id, key string, revision int64, disposition string) {
	t.Helper()
	if _, err := f.engine.DecideCLIFinding(t.Context(), f.scope(t, f.writer, false), key, id, reviewexec.CLIDecisionRequest{ExpectedRevision: &revision, Finding: 0, Disposition: disposition, Reason: "Explicit synthetic decision"}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIDriverPendingEvidenceBlocksRepeatSpendAcrossHeads(t *testing.T) {
	for _, phase := range []string{"unadjudicated", "unfinalized", "accepted-unfixed", "moved-head", "stacked-predecessor"} {
		t.Run(phase, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			_, id := cliDriverCompletedFinding(t, f)
			if phase == "unfinalized" || phase == "accepted-unfixed" {
				cliDriverDecision(t, f, id, "accept-pending", 0, "accepted")
			}
			if phase == "accepted-unfixed" {
				revision := int64(1)
				if _, err := f.engine.FinalizeCLI(t.Context(), f.scope(t, f.writer, false), "finalize-pending", id, reviewexec.CLIFinalizeRequest{ExpectedRevision: &revision}); err != nil {
					t.Fatal(err)
				}
			}
			number, revision := int64(2), int64(1)
			if phase == "moved-head" {
				p := f.reader.pulls[2]
				p.HeadSHA = strings.Repeat("9", 40)
				f.reader.pulls[2] = p
			}
			if phase == "stacked-predecessor" {
				f.configure(t, "stacked", []int64{2, 4})
				number, revision = 4, 2
				p := f.reader.pulls[4]
				p.BaseSHA, p.BaseRef = f.headSHA, "parent"
				f.reader.pulls[4] = p
			}
			before, _ := os.ReadFile(f.log)
			fetches := f.fetches.Load()
			op, a, c := f.start(t, number, revision, "new-explicit-intent")
			_, err := f.driver.Prepare(t.Context(), op, c, a)
			var failure *core.Error
			want := "review_evidence_pending"
			if phase == "stacked-predecessor" {
				want = "review_held"
			}
			if !errors.As(err, &failure) || failure.Code != want {
				t.Fatal("pending source did not hold", phase, err)
			}
			if phase == "stacked-predecessor" {
				details, ok := failure.Details.(map[string]string)
				if !ok || details["reason"] != "predecessor_unfinished" {
					t.Fatal(failure)
				}
			}
			after, _ := os.ReadFile(f.log)
			if string(after) != string(before) || f.fetches.Load() != fetches {
				t.Fatal("pending evidence reached fetch, usage or review")
			}
		})
	}
}

func TestCLIDriverLatePreparationCorrectionNeverRunsReview(t *testing.T) {
	f := newCLIDriverFixture(t)
	_, id := cliDriverCompletedFinding(t, f)
	cliDriverDecision(t, f, id, "reject-source", 0, "rejected")
	revision := int64(1)
	if _, err := f.engine.FinalizeCLI(t.Context(), f.scope(t, f.writer, false), "finalize-source", id, reviewexec.CLIFinalizeRequest{ExpectedRevision: &revision}); err != nil {
		t.Fatal(err)
	}
	// The same Git objects under a different explicit base comparison have no
	// current coverage. Correct an older closed finding after Prepare's first
	// comparison read; Dispatch must still refuse before invoking review.
	f.reader.repository.DefaultBranch = "new-main"
	p := f.reader.pulls[2]
	p.BaseRef = "new-main"
	f.reader.pulls[2] = p
	usage := f.driver.usage
	f.driver.usage = func(ctx context.Context, w reviewworkspace.Workspace, repo, org string) (reviewprocess.Usage, error) {
		cliDriverDecision(t, f, id, "late-accept", 2, "accepted")
		return usage(ctx, w, repo, org)
	}
	op, a := f.prepareAdmit(t, 2, 1, "changed-comparison")
	before, _ := os.ReadFile(f.log)
	result, err := f.driver.Dispatch(t.Context(), op, a, nil)
	if err != nil || result.State != "failed" || result.Reason != "source_admission_changed" {
		t.Fatal("late pending evidence did not refuse action", result, err)
	}
	after, _ := os.ReadFile(f.log)
	if string(before) != string(after) {
		t.Fatal("late correction spent another review")
	}
}

func TestCLIDriverPredecessorCorrectionAfterGitVerificationStopsReview(t *testing.T) {
	for _, correct := range []bool{false, true} {
		t.Run(fmt.Sprint(correct), func(t *testing.T) {
			f := newCLIDriverFixture(t)
			_, id := cliDriverCompletedFinding(t, f)
			cliDriverDecision(t, f, id, "reject-parent", 0, "rejected")
			revision := int64(1)
			if _, err := f.engine.FinalizeCLI(t.Context(), f.scope(t, f.writer, false), "finalize-parent", id, reviewexec.CLIFinalizeRequest{ExpectedRevision: &revision}); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(filepath.Dir(f.storeDir), "fixture-source")
			if err := os.WriteFile(filepath.Join(source, "third.txt"), []byte("child comparison\n"), 0600); err != nil {
				t.Fatal(err)
			}
			gitTestRun(t, source, "add", "third.txt")
			gitTestRun(t, source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "child")
			third := gitTestOutput(t, source, "rev-parse", "HEAD")
			gitTestRun(t, source, "push", "cache", "HEAD:refs/heads/third")
			merged := f.reader.pulls[3]
			merged.Merged, merged.State = true, "closed"
			f.reader.pulls[3] = merged
			child := f.reader.pulls[4]
			child.HeadSHA, child.BaseSHA, child.BaseRef = third, f.headSHA, "covered-parent"
			f.reader.pulls[4] = child
			f.configure(t, "stacked", []int64{2, 3, 4})
			op, a := f.prepareAdmit(t, 4, 2, "child-after-covered-parent")
			var verifications, reviews atomic.Int64
			f.driver.verify = func(ctx context.Context, w reviewworkspace.Workspace) error {
				if err := f.manager.Verify(ctx, w); err != nil {
					return err
				}
				verifications.Add(1)
				if correct {
					cliDriverDecision(t, f, id, "correct-parent-after-git", 2, "accepted")
				}
				return nil
			}
			f.driver.review = func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error) {
				reviews.Add(1)
				return reviewprocess.Capture{ExitCode: -1}, &reviewprocess.Error{Code: "process_failed"}
			}
			before, _ := os.ReadFile(f.log)
			result, err := f.driver.Dispatch(t.Context(), op, a, nil)
			if err != nil || verifications.Load() != 1 {
				t.Fatal("dispatch failed before real final Git verification", result, err)
			}
			wantReviews, wantReason := int64(1), "process_not_started"
			if correct {
				wantReviews, wantReason = 0, "admission_changed"
			}
			if result.State != "failed" || result.Reason != wantReason || reviews.Load() != wantReviews {
				t.Fatal("late predecessor correction crossed final spend boundary", result, reviews.Load())
			}
			after, _ := os.ReadFile(f.log)
			if string(before) != string(after) {
				t.Fatal("fixture invoked unexpected CLI executable")
			}
			if f.reader.repositoryCalls != 4 || f.reader.calls[2] != 4 || f.reader.calls[3] != 2 || f.reader.calls[4] != 2 {
				t.Fatal("final admission duplicated provider source reads", f.reader.calls, f.reader.repositoryCalls)
			}
			// The corrected predecessor is still the original finalized source count;
			// only its current accepted-without-fix obligation changed.
			if correct {
				state, err := f.engine.ReadCLIAdjudication(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), id)
				if err != nil || !state.Finalized || state.FrozenCounts.Accepted != 0 || state.Findings[0].Disposition != "accepted" || state.Findings[0].FixSHA != "" {
					t.Fatal("regression did not commit the actual correction", err)
				}
			}
		})
	}
}

func TestCLIDriverRetainedEvidenceSurvivesMissingCurrentProjection(t *testing.T) {
	for _, mode := range []string{"missing_current_pull", "publication_storage_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			review := f.driver.review
			hookResult := make(chan error, 1)
			f.driver.review = func(ctx context.Context, w reviewworkspace.Workspace) (reviewprocess.Capture, error) {
				capture, err := review(ctx, w)
				if err != nil {
					return capture, err
				}
				if mode == "publication_storage_failure" {
					_, err = f.store.DB().Exec(`CREATE TRIGGER fail_cli_native_publication BEFORE INSERT ON review_runs WHEN NEW.source='native-executor' BEGIN SELECT RAISE(ABORT,'synthetic publication storage failure'); END`)
				} else {
					// Replace only the current browser observation using the public importer;
					// historical preparation metadata and the authoritative ledger survive.
					var revision int64
					err = f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision)
					if err == nil {
						input := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 901, Source: "synthetic-current-retirement", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "superseded"}}}
						body, marshalErr := json.Marshal(input)
						err = marshalErr
						if err == nil {
							mux := http.NewServeMux()
							reviews.Register(mux, f.store)
							request := httptest.NewRequest("POST", "/api/v1/projects/"+f.project+"/reviews/observations", strings.NewReader(string(body)))
							request.Header.Set("Authorization", "Bearer "+f.admin)
							request.Header.Set("Idempotency-Key", "retire-current-during-review")
							response := httptest.NewRecorder()
							mux.ServeHTTP(response, request)
							if response.Code != http.StatusCreated {
								err = fmt.Errorf("synthetic retirement status%d", response.Code)
							}
						}
					}
				}
				hookResult <- err
				return capture, err
			}
			request := reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: f.headSHA}
			raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "ledger-only-supervised-review", request)
			if err != nil {
				t.Fatal(err)
			}
			var receipt reviewexec.Receipt
			if err = json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: f.driver})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var stopOnce sync.Once
			stop := func() {
				stopOnce.Do(func() {
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error("supervisor stop", err)
						}
					case <-time.After(3 * time.Second):
						t.Error("supervisor did not stop")
					}
				})
			}
			defer stop()
			var operation reviewexec.Operation
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
		settled:
			for {
				operation, err = f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID)
				if err != nil {
					t.Fatal(err)
				}
				switch operation.State {
				case "completed", "uncertain", "failed", "held", "cancelled":
					break settled
				}
				select {
				case <-deadline.C:
					t.Fatal("supervisor did not settle retained source")
				case <-tick.C:
				}
			}
			stop()
			select {
			case err := <-hookResult:
				if err != nil {
					t.Fatal("deterministic publication fixture failed", err)
				}
			default:
				t.Fatal("real review hook never ran")
			}
			want, reservations := "completed", 0
			if mode == "publication_storage_failure" {
				want, reservations = "uncertain", 1
			}
			if operation.State != want || mode == "missing_current_pull" && operation.Reason != "capture_retained" || mode == "publication_storage_failure" && operation.Reason != "capture_retention_failed" {
				t.Fatal("publication error changed retained execution outcome", operation.State, operation.Reason)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, operation.AttemptID) != reservations || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, operation.AttemptID) != 1 {
				t.Fatal("settlement lost ledger or reservation policy")
			}
			var evidenceID string
			if err = f.store.DB().QueryRow(`SELECT id FROM review_execution_evidence WHERE attempt_id=?`, operation.AttemptID).Scan(&evidenceID); err != nil {
				t.Fatal(err)
			}
			evidence, err := f.engine.ReadCLIEvidence(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), evidenceID)
			if err != nil || !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
				t.Fatal("authoritative capture lost", err)
			}
			adjudication, err := f.engine.ReadCLIAdjudication(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), evidenceID)
			if err != nil || adjudication.Finalized || len(adjudication.Findings) != 1 || adjudication.Findings[0].Disposition != "unadjudicated" {
				t.Fatal("publication failure fabricated finding disposition/coverage", err)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_runs WHERE project_id=? AND source='native-executor' AND source_run_id=?`, f.project, evidenceID) != 0 {
				t.Fatal("driver invented a native browser row")
			}
			if mode == "missing_current_pull" {
				if cliCount(t, f.store, `SELECT count(*) FROM review_workspace w JOIN review_pulls p ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=? AND p.number=2`, f.project) != 0 {
					t.Fatal("driver recreated retired current metadata")
				}
				comparison, err := f.engine.CLIComparison(t.Context(), f.project, reviewexec.SourcePlan{Repository: f.reader.repository, Pull: f.reader.pulls[2], ParentSHA: f.baseSHA, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
				if err != nil || !comparison.Pending || comparison.Covered {
					t.Fatal("completed ledger-only evidence escaped pending coverage", comparison, err)
				}
			}
			log, err := os.ReadFile(f.log)
			if err != nil || strings.Count(string(log), "usage --agent\n") != 1 || strings.Count(string(log), "review --agent --committed ") != 1 {
				t.Fatal("review work was duplicated", err)
			}
			replay, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "ledger-only-supervised-review", request)
			if err != nil || string(replay) != string(raw) || cliCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
				t.Fatal("original request replay changed effect", err)
			}
		})
	}
}
