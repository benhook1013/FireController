package reviewruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestCLIQuotaRetryRequalifiesOriginalComparisonBeforeAnotherUsageCheck(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "moved-base"
		}
		t.Run(name, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			now := time.Now().UTC()
			var err error
			f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection(), reviewexec.WithExecutionClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			f.driver.engine = f.engine
			calls := 0
			usage := f.driver.usage
			f.driver.usage = func(ctx context.Context, workspace reviewworkspace.Workspace, repo, org string) (reviewprocess.Usage, error) {
				calls++
				if calls == 1 {
					return reviewprocess.Usage{Limit: 3, RollingWindowMS: 3600000}, &reviewprocess.Error{Code: "rate_limited"}
				}
				return usage(ctx, workspace, repo, org)
			}
			op, a, c := f.start(t, 2, 1, "quota-pinned-request")
			_, err = f.driver.Prepare(t.Context(), op, c, a)
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "review_held" {
				t.Fatal("first quota observation", err)
			}
			if _, err = f.engine.ScheduleCLICooldown(t.Context(), f.project, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			queued, err := f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), op.ID)
			if err != nil || queued.Wait == nil {
				t.Fatal("retained request", queued, err)
			}
			now, err = time.Parse(time.RFC3339Nano, queued.Wait.RetryNotBefore)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				cache, err := f.manager.CachePath(901)
				if err != nil {
					t.Fatal(err)
				}
				gitOutput := func(args ...string) string {
					cmd := exec.Command("git", args...)
					cmd.Dir = cache
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("fixture git: %s: %v", out, err)
					}
					return strings.TrimSpace(string(out))
				}
				tree := gitOutput("rev-parse", f.baseSHA+"^{tree}")
				next := gitOutput("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-p", f.baseSHA, "-m", "Advance base without changing the reviewed patch")
				p := f.reader.pulls[2]
				p.BaseSHA = next
				f.reader.pulls[2] = p
			}
			next, err := f.engine.StartAttempt(t.Context(), f.project, op.ID)
			if err != nil || next.ID == "" {
				t.Fatal("retry claim", next, err)
			}
			current, err := f.engine.ExecutionContext(t.Context(), f.project, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := f.driver.Prepare(t.Context(), current.Operation, current.Configuration, next)
			if changed {
				if !errors.As(err, &failure) || failure.Code != "review_held" {
					t.Fatal("changed comparison was allowed", err)
				}
				reason, _ := failure.Details.(map[string]string)
				if reason["reason"] != "source_changed" || calls != 1 {
					t.Fatal("moved base reached quota query", reason, calls)
				}
				return
			}
			if err != nil || calls != 2 {
				t.Fatal("unchanged request failed requalification", err, calls)
			}
			admitted, err := f.engine.Admit(t.Context(), f.project, next.ID, next.Revision, prepared.Anchor, prepared.Intent)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.driver.Dispatch(t.Context(), current.Operation, admitted, nil)
			if err != nil || result.State != "completed" {
				t.Fatal("original request did not execute once", result, err)
			}
			if _, err = f.engine.Advance(t.Context(), f.project, admitted.ID, admitted.Revision, result.State, result.Reason, ""); err != nil {
				t.Fatal(err)
			}
			if err := reviewexec.ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err != nil {
				t.Fatal("completed retry damaged original quota evidence", err)
			}
		})
	}
}

func TestCLIBaselineStorageFailureRetainsPreparation(t *testing.T) {
	f := newCLIDriverFixture(t)
	op, attempt, config := f.start(t, 2, 1, "baseline-storage-failure")
	f.driver.verify = func(ctx context.Context, w reviewworkspace.Workspace) error {
		if err := f.manager.Verify(ctx, w); err != nil {
			return err
		}
		return f.store.Close()
	}
	before, _ := os.ReadFile(f.log)
	_, err := f.driver.Prepare(t.Context(), op, config, attempt)
	var recovery *reviewexec.PreparationRecoveryError
	if !errors.As(err, &recovery) {
		t.Fatal("baseline storage failure became terminal", err)
	}
	after, _ := os.ReadFile(f.log)
	if string(before) != string(after) {
		t.Fatal("baseline failure requested allowance/provider work")
	}
}

func TestCLIExhaustionArtifactRecoversPreSQLCrashWithoutDriver(t *testing.T) {
	f := newCLIDriverFixture(t)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_cli_exhaustion BEFORE INSERT ON review_cli_preflight BEGIN SELECT RAISE(FAIL,'temporary outage'); END`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.driver.usage = func(context.Context, reviewworkspace.Workspace, string, string) (reviewprocess.Usage, error) {
		calls++
		return reviewprocess.Usage{Limit: 3, RollingWindowMS: 3600000}, &reviewprocess.Error{Code: "rate_limited"}
	}
	op, a, c := f.start(t, 2, 1, "durable-exhaustion")
	_, err := f.driver.Prepare(t.Context(), op, c, a)
	var recovery *reviewexec.PreparationRecoveryError
	if !errors.As(err, &recovery) {
		t.Fatal("outage did not preserve replay", err)
	}
	r, found, err := f.driver.exhaustions.LoadCLIExhaustion(t.Context(), a.ID)
	if err != nil || !found {
		t.Fatal("no pre-SQL artifact", err)
	}
	if _, err = f.store.DB().Exec(`DROP TRIGGER fail_cli_exhaustion`); err != nil {
		t.Fatal(err)
	}
	// Simulate restart with no catalog, executable, provider or workspace manager.
	restarted, err := reviewexec.New(f.store, nil, reviewexec.WithCLIExhaustionRecovery(f.driver.exhaustions))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := restarted.Start(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	got, err := restarted.ReadOperation(t.Context(), f.scope(t, f.writer, false), op.ID)
	if err != nil || got.State != "queued" || got.ID != op.ID || got.Wait == nil || got.Wait.BaselineAttemptID != a.ID {
		t.Fatal("original request not recovered", got, err)
	}
	var body string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&body); err != nil || !strings.Contains(body, r.Observation.ObservedAt) {
		t.Fatal("observation time lost", err)
	}
	var eventsBefore, eventsAfter int
	f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&eventsBefore)
	if err = restarted.ReplayCLIExhaustion(t.Context(), r); err != nil {
		t.Fatal("exact replay", err)
	}
	f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&eventsAfter)
	if eventsBefore != eventsAfter || calls != 1 {
		t.Fatal("replay duplicated work", eventsBefore, eventsAfter, calls)
	}
	var credit int
	f.store.DB().QueryRow(`SELECT count(*) FROM review_execution_evidence`).Scan(&credit)
	if credit != 0 {
		t.Fatal("exhaustion fabricated review credit")
	}
	if _, err = f.store.DB().Exec(`INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id) VALUES(?,?,?,'cli',?,?)`, a.Provider, r.Observation.Anchor.RepositoryID, op.PullNumber, a.Account, a.ID); err != nil {
		t.Fatal(err)
	}
	if err = restarted.ReplayCLIExhaustion(t.Context(), r); err == nil {
		t.Fatal("exact replay accepted a retained reservation")
	}
}

func TestCLIExhaustionArtifactMissingInvalidAndAgedSource(t *testing.T) {
	for _, scenario := range []string{"missing", "aged", "tampered", "binding", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			op, a, _ := f.start(t, 2, 1, "artifact-"+scenario)
			anchor := reviewexec.Anchor{RepositoryID: 901, Repository: "owner/repo", PullNumber: 2, CandidateSHA: op.HeadSHA, BaseSHA: f.baseSHA, ParentSHA: f.baseSHA, BaseRef: "main", MergeBaseSHA: f.baseSHA, PatchSHA256: strings.Repeat("a", 64), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			r := reviewexec.CLIExhaustionRecovery{Operation: op, Attempt: a, Observation: reviewexec.CLIAllowanceObservation{Anchor: anchor, ObservedAt: anchor.ObservedAt, Remaining: 0, Limit: 3, RollingWindowMS: 3600000, ChangedFiles: 1}}
			source := f.driver.exhaustions
			if scenario == "aged" {
				old := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
				r.Observation.ObservedAt = old
				r.Observation.Anchor.ObservedAt = old
			}
			if scenario == "binding" {
				r.Operation.PolicyRevision++
			}
			if scenario != "missing" && scenario != "unavailable" {
				if err := source.Save(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "tampered" {
				path := filepath.Join(source.root, a.ID+".json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.Replace(data, []byte(`"remaining":0`), []byte(`"remaining":1`), 1)
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unavailable" {
				if err := os.WriteFile(source.root, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := reviewexec.New(f.store, nil, reviewexec.WithCLIExhaustionRecovery(source))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done, err := restarted.Start(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			<-done
			got, err := restarted.ReadOperation(t.Context(), f.scope(t, f.writer, false), op.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "cancelled"
			if scenario == "aged" {
				want = "queued"
			}
			if scenario == "unavailable" {
				want = "preparing"
			}
			if got.State != want {
				t.Fatal("wrong source recovery outcome", scenario, got.State)
			}
			var reservations int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID).Scan(&reservations); err != nil {
				t.Fatal(err)
			}
			expectedReservations := 0
			if scenario == "unavailable" {
				expectedReservations = 1
			}
			if reservations != expectedReservations {
				t.Fatal("wrong reservation recovery", scenario, reservations)
			}
			if scenario == "aged" {
				due, err := time.Parse(time.RFC3339Nano, got.Wait.RetryNotBefore)
				if err != nil || !due.After(time.Now()) {
					t.Fatal("aged original proof did not rearm from recovery", due, err)
				}
			}
		})
	}
}

func TestCLIExhaustionSaveRetriesBounded(t *testing.T) {
	for _, tc := range []struct {
		name           string
		failures, want int
		refusal        bool
	}{
		{"transient", 1, 2, false}, {"exhausted", 5, 3, false}, {"conflict", 5, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryCLIExhaustionSave(t.Context(), func(context.Context, time.Duration) error { return nil }, func() error {
				calls++
				if calls <= tc.failures {
					if tc.refusal {
						return captureError("capture_conflict")
					}
					return captureError("capture_unavailable")
				}
				return nil
			})
			if calls != tc.want || (err == nil) != (tc.failures < tc.want) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestCLIExhaustionSaveRetryReconcilesPublishedArtifact(t *testing.T) {
	f := newCLIDriverFixture(t)
	op, a, _ := f.start(t, 2, 1, "save-replay")
	anchor := reviewexec.Anchor{RepositoryID: 901, Repository: "owner/repo", PullNumber: 2, CandidateSHA: op.HeadSHA, BaseSHA: f.baseSHA, ParentSHA: f.baseSHA, BaseRef: "main", MergeBaseSHA: f.baseSHA, PatchSHA256: strings.Repeat("a", 64), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	record := reviewexec.CLIExhaustionRecovery{Operation: op, Attempt: a, Observation: reviewexec.CLIAllowanceObservation{Anchor: anchor, ObservedAt: anchor.ObservedAt, Remaining: 0, Limit: 3, RollingWindowMS: 3600000, ChangedFiles: 1}}
	calls := 0
	err := retryCLIExhaustionSave(t.Context(), func(context.Context, time.Duration) error { return nil }, func() error {
		calls++
		if err := f.driver.exhaustions.Save(t.Context(), record); err != nil {
			return err
		}
		if calls == 1 {
			return captureError("capture_unavailable")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatal("failed to reconcile identical published record", calls, err)
	}
	got, found, err := f.driver.exhaustions.LoadCLIExhaustion(t.Context(), a.ID)
	if err != nil || !found || got.Attempt.ID != a.ID {
		t.Fatal(got, found, err)
	}
}
