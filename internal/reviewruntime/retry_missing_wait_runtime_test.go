package reviewruntime

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestExpiredCLIWaitDeletionCannotPrepareChangedComparison(t *testing.T) {
	f := newCLIDriverFixture(t)
	now := time.Now().UTC()
	var err error
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection(), reviewexec.WithExecutionClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	f.driver.engine = f.engine
	usageCalls := 0
	usage := f.driver.usage
	f.driver.usage = func(ctx context.Context, w reviewworkspace.Workspace, repo, org string) (reviewprocess.Usage, error) {
		usageCalls++
		if usageCalls == 1 {
			return reviewprocess.Usage{Limit: 3, RollingWindowMS: 3600000}, &reviewprocess.Error{Code: "rate_limited"}
		}
		return usage(ctx, w, repo, org)
	}
	op, a, c := f.start(t, 2, 1, "missing-wait-runtime")
	if _, err = f.driver.Prepare(t.Context(), op, c, a); err == nil {
		t.Fatal("expected exhaustion")
	}
	if _, err = f.engine.ScheduleCLICooldown(t.Context(), f.project, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	queued, err := f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), op.ID)
	if err != nil || queued.Wait == nil {
		t.Fatal(queued, err)
	}
	now, err = time.Parse(time.RFC3339Nano, queued.Wait.RetryNotBefore)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	var retained string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`DELETE FROM review_operation_waits WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	cache, err := f.manager.CachePath(901)
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = cache
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git: %s %v", out, err)
		}
		return strings.TrimSpace(string(out))
	}
	tree := git("rev-parse", f.baseSHA+"^{tree}")
	nextBase := git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-p", f.baseSHA, "-m", "Advance comparison base")
	pull := f.reader.pulls[2]
	pull.BaseSHA = nextBase
	f.reader.pulls[2] = pull
	next, err := f.engine.StartAttempt(t.Context(), f.project, op.ID)
	if err == nil || next.ID != "" || usageCalls != 1 {
		t.Fatal("missing baseline was not fenced before preparation", next, err, usageCalls)
	}
	var after string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&after); err != nil || after != retained {
		t.Fatal("original comparison evidence changed", err)
	}
}
