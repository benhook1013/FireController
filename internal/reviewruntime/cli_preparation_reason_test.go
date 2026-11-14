package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestCLIDriverUsageFailureBecomesSafePreparationHold(t *testing.T) {
	for _, tc := range []struct {
		name          string
		failure       error
		usage         reviewprocess.Usage
		failRetention bool
		want          string
	}{
		{"rate", &reviewprocess.Error{Code: "rate_limited"}, reviewprocess.Usage{}, false, "allowance_unavailable"},
		{"wrapped rate", fmt.Errorf("private path or token: %w", &reviewprocess.Error{Code: "rate_limited"}), reviewprocess.Usage{}, false, "allowance_unavailable"},
		{"usage", &reviewprocess.Error{Code: "usage_unavailable"}, reviewprocess.Usage{}, false, "allowance_unavailable"},
		{"wrapped usage", fmt.Errorf("private path: %w", &reviewprocess.Error{Code: "usage_unavailable"}), reviewprocess.Usage{}, false, "allowance_unavailable"},
		{"raw text", errors.New("rate_limited private token diagnostic"), reviewprocess.Usage{}, false, "allowance_unavailable"},
		{"foreign typed code", core.Fail("rate_limited", "private diagnostic", 409), reviewprocess.Usage{}, false, "allowance_unavailable"},
		{"rate with nonzero remaining", &reviewprocess.Error{Code: "rate_limited"}, reviewprocess.Usage{Remaining: 1, Limit: 3, RollingWindowMS: 60000}, false, "allowance_unavailable"},
		{"exhaustion file limit", &reviewprocess.Error{Code: "rate_limited"}, reviewprocess.Usage{Limit: 3, RollingWindowMS: 60000}, false, "project_file_limit"},
		{"exhaustion validation fails", &reviewprocess.Error{Code: "rate_limited"}, reviewprocess.Usage{Limit: -1, RollingWindowMS: 60000}, false, "allowance_unavailable"},
		{"exhaustion retention fails", &reviewprocess.Error{Code: "rate_limited"}, reviewprocess.Usage{Limit: 3, RollingWindowMS: 60000}, true, "allowance_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			var usage atomic.Int64
			f.driver.usage = func(context.Context, reviewworkspace.Workspace, string, string) (reviewprocess.Usage, error) {
				usage.Add(1)
				if tc.name == "exhaustion file limit" {
					// Force the retained-policy guard after the workspace's earlier
					// ceiling check, isolating RecordCLIExhaustion's typed refusal.
					if _, err := f.store.DB().Exec(`UPDATE review_policy_config SET body_json=json_set(body_json,'$.max_review_files',1)`); err != nil {
						t.Fatal(err)
					}
				}
				return tc.usage, tc.failure
			}
			op, a, c := f.start(t, 2, 1, "prepare-usage-refusal")
			if tc.failRetention {
				if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_cli_exhaustion BEFORE INSERT ON review_cli_preflight BEGIN SELECT RAISE(ABORT,'private-exhaustion-storage-failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := f.driver.Prepare(t.Context(), op, c, a)
			var failure *core.Error
			var recovery *reviewexec.PreparationRecoveryError
			if tc.failRetention {
				if !errors.As(err, &recovery) || strings.Contains(err.Error(), "private") || prepared.Anchor != (reviewexec.Anchor{}) {
					t.Fatalf("retention failure lost recoverable preparation: %T %v", err, err)
				}
				if _, found, loadErr := f.driver.exhaustions.LoadCLIExhaustion(t.Context(), a.ID); loadErr != nil || !found {
					t.Fatal("retention failure lost original private source", loadErr)
				}
			} else if !errors.As(err, &failure) || failure.Code != "review_held" {
				t.Fatalf("usage error lost safe hold: %T %v", err, err)
			}
			if !tc.failRetention {
				details, ok := failure.Details.(map[string]string)
				if !ok || details["reason"] != tc.want || strings.Contains(failure.Error(), "private") || prepared.Anchor != (reviewexec.Anchor{}) || errors.As(err, &recovery) {
					t.Fatal("bad safe refusal", failure, prepared)
				}
			}
			if usage.Load() != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("refused allowance retained permission/evidence")
			}
			f.driver.mu.Lock()
			_, exists := f.driver.prepared[a.ID]
			f.driver.mu.Unlock()
			if exists {
				t.Fatal("failed preparation left dispatch entry")
			}
		})
	}
}

func TestCLIDriverRealUsageHoldSettlesSupervisorWithoutReview(t *testing.T) {
	for _, mode := range []string{"exhausted", "unavailable", "repository_changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			expected := "allowance_exhausted"
			if mode == "repository_changed" {
				f.reader.repository.ID = 999
				expected = "repository_changed"
			} else {
				body := `{"type":"status","phase":"usage","status":"complete","organization":"fixture-org","includedReviews":{"state":"available","repository":"owner/repo","remaining":0,"limit":3,"fullCapacityInMs":0,"rollingWindowMs":60000},"billingPeriod":{"state":"available","usageBillingStatus":"inactive"}}`
				if mode == "unavailable" {
					body = "private invalid provider diagnostic"
					expected = "allowance_unavailable"
				}
				// The real Runner executes this allowlisted local Usage process. It emits no
				// network calls and would log any accidental review invocation.
				script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + cliShellQuote(f.log) + "\nprintf '%s\\n' " + cliShellQuote(body) + "\n"
				if err := os.WriteFile(f.capability.CLIExecutable, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var reviews atomic.Int64
			f.driver.review = func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error) {
				reviews.Add(1)
				return reviewprocess.Capture{}, errors.New("review must not execute")
			}
			raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "pipeline-preparation-reason", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: f.headSHA})
			if err != nil {
				t.Fatal(err)
			}
			var receipt reviewexec.Receipt
			if json.Unmarshal(raw, &receipt) != nil {
				t.Fatal(string(raw))
			}
			ctx, cancel := context.WithCancel(t.Context())
			done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: f.driver})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var once sync.Once
			stop := func() {
				once.Do(func() {
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(3 * time.Second):
						t.Error("supervisor did not stop")
					}
				})
			}
			defer stop()
			deadline := time.NewTimer(4 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			var operation reviewexec.Operation
			for {
				operation, err = f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID)
				if err != nil {
					t.Fatal(err)
				}
				if operation.State == "held" || mode == "exhausted" && operation.State == "queued" && operation.Wait != nil && operation.Wait.BaselineAttemptID != "" && operation.Wait.RetryNotBefore != "" {
					break
				}
				select {
				case <-deadline.C:
					t.Fatal("supervisor failed to settle refusal", operation)
				case <-tick.C:
				}
			}
			stop()
			expectedPreflight := 0
			if mode == "exhausted" {
				expectedPreflight = 1
				if operation.State != "queued" || operation.Wait == nil || operation.Wait.RetryNotBefore == "" {
					t.Fatal("real runner exhaustion did not retain queued wait", operation)
				}
				expected = ""
			}
			if operation.Reason != expected || reviews.Load() != 0 || strings.Contains(operation.Reason, "private") {
				t.Fatal("safe hold reason missing or review dispatched", operation, reviews.Load())
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`) != expectedPreflight {
				t.Fatal("preparation hold retained permission/reservation/evidence")
			}
			log, err := os.ReadFile(f.log)
			if mode == "repository_changed" {
				if !os.IsNotExist(err) || f.fetches.Load() != 0 {
					t.Fatal("repository mismatch reached allowance/fetch", err, string(log))
				}
			} else if err != nil || string(log) != "usage --agent\n" {
				t.Fatal("usage repeated/review invoked", string(log), err)
			}
		})
	}
}

func TestCLIExhaustionDirectoriesSyncsExistingAndCreatedParents(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "recovery", "exhaustions")
	var synced []string
	syncParent := func(path string) error { synced = append(synced, path); return syncCaptureDirectory(path) }
	if err := prepareCLIExhaustionDirectories(root, base, syncParent); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 2 || synced[len(synced)-2] != base || synced[len(synced)-1] != filepath.Join(base, "recovery") {
		t.Fatalf("directory names were not synced in ancestor order: %v", synced)
	}
	first := append([]string(nil), synced...)
	synced = nil
	if err := prepareCLIExhaustionDirectories(root, base, syncParent); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(synced, first) {
		t.Fatalf("existing directory names not reconciled: %v, want %v", synced, first)
	}
	if err := captureDirectories(root, false); err != nil {
		t.Fatal(err)
	}
}

func TestCLIExhaustionDirectoriesDoesNotHideParentSyncFailure(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "recovery", "exhaustions")
	failure := errors.New("parent sync unavailable")
	calls := 0
	err := prepareCLIExhaustionDirectories(root, base, func(string) error { calls++; return failure })
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("sync error lost: %v, calls %d", err, calls)
	}
}

func TestCLIExhaustionDirectoriesRetriesUncertainDirectoryName(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "recovery", "exhaustions")
	failure := errors.New("parent sync unavailable")
	err := prepareCLIExhaustionDirectories(root, base, func(path string) error {
		if path == base {
			return failure
		}
		return syncCaptureDirectory(path)
	})
	if !errors.Is(err, failure) {
		t.Fatalf("sync error lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "recovery")); err != nil {
		t.Fatalf("uncertain directory not created: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("continued beyond uncertain creation: %v", err)
	}
	reconciled := false
	if err := prepareCLIExhaustionDirectories(root, base, func(path string) error {
		if path == base {
			reconciled = true
		}
		if path == filepath.Join(base, "recovery") && !reconciled {
			t.Fatal("created child before reconciling parent")
		}
		return syncCaptureDirectory(path)
	}); err != nil {
		t.Fatal(err)
	}
	if !reconciled {
		t.Fatal("retry skipped existing directory's parent sync")
	}
}

func TestCLIExhaustionStoreResolvesAncestorBeforeLazyDirectories(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store, err := NewCLIExhaustionStore(filepath.Join(link, "recovery", "exhaustions"), link)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "recovery", "exhaustions")
	if store.root != want {
		t.Fatalf("unresolved lazy path: %s, want %s", store.root, want)
	}
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("constructor created storage: %v", err)
	}
	if err := prepareCLIExhaustionDirectories(store.root, store.boundary, syncCaptureDirectory); err != nil {
		t.Fatal(err)
	}
	if err := captureDirectories(store.root, false); err != nil {
		t.Fatal(err)
	}
}

func TestCLIExhaustionManagedBoundarySurvivesReconstruction(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "recovery", "exhaustions")
	injected := errors.New("uncertain parent sync")
	if err := prepareCLIExhaustionDirectories(root, base, func(path string) error { return injected }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	// A restart keeps the configured custody boundary rather than trusting the
	// deepest directory that happens to exist after the interrupted creation.
	store, err := NewCLIExhaustionStore(root, base)
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	if err := prepareCLIExhaustionDirectories(store.root, store.boundary, func(path string) error {
		if path != base && path != filepath.Join(base, "recovery") {
			t.Fatal("sync escaped managed boundary", path)
		}
		synced = append(synced, path)
		return syncCaptureDirectory(path)
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(synced, []string{base, filepath.Join(base, "recovery")}) {
		t.Fatal("restart skipped uncertain parent", synced)
	}
}

func TestCLIExhaustionExecuteOnlyOutsideManagedBoundary(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "data")
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0111); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	root := filepath.Join(base, "exhaustions")
	store, err := NewCLIExhaustionStore(root, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCLIExhaustionDirectories(store.root, store.boundary, syncCaptureDirectory); err != nil {
		t.Fatal("unrelated execute-only ancestor blocked recovery", err)
	}
}
