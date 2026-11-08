package reviewruntime

import (
	"context"
	"errors"
	"os/exec"
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
