package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type draftWaitDriver struct {
	controlledDriver
	reader TargetReader
}

func (d *draftWaitDriver) SourceReader() TargetReader { return d.reader }

func TestExplicitCLIDraftWaitReusesIntentAndLeavesIndependentWorkEligible(t *testing.T) {
	f := supervisorFixture(t)
	now := time.Now()
	f.e.schedulingNow = func() time.Time { return now }
	draft := true
	d := &draftWaitDriver{reader: &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		isDraft := number == 2 && draft
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &isDraft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}}
	drivers := map[string]Driver{"test-capability": d}
	request := f.enqueue(t, attemptProject, "cli-wait-original", "cli", 2, 1)
	run := func() {
		t.Helper()
		if err := f.e.processNext(t.Context(), drivers); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id string) Operation {
		t.Helper()
		op, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), id)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	run()
	waiting := read(request.ID)
	if waiting.State != "queued" || waiting.Reason != cliDraftWait || waiting.AttemptID != "" || waiting.Wait != nil || d.prepares.Load() != 0 || attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
		t.Fatal("Draft consumed preparation or quota", waiting)
	}
	if replay := f.enqueue(t, attemptProject, "cli-wait-original", "cli", 2, 1); replay.ID != request.ID {
		t.Fatal("same key created another operation", replay)
	}
	other := f.enqueue(t, attemptProject, "cli-wait-independent", "cli", 4, 1)
	now = now.Add(cliDraftProbeInterval)
	run()
	if read(other.ID).State != "completed" || read(request.ID).Revision != waiting.Revision || d.calls.Load() != 1 {
		t.Fatal("waiting Draft blocked independent work or churned its state")
	}

	control := func(action string) {
		t.Helper()
		_, err := f.e.ControlOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), "cli-wait-"+action, request.ID, OperationControl{ExpectedRevision: read(request.ID).Revision, Action: action})
		if err != nil {
			t.Fatal(err)
		}
	}
	control("pause")
	draft = false
	now = now.Add(cliDraftProbeInterval)
	run()
	if read(request.ID).State != "queued" || d.calls.Load() != 1 {
		t.Fatal("ready PR bypassed queued pause")
	}
	control("resume")
	// A new engine has no in-process readiness cache and sees the durable
	// queued request through the same recovery entry point used at startup.
	fresh, err := New(f.store, attemptCapabilities, WithExecutionClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	f.e = fresh
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if got := read(request.ID); got.State != "completed" || got.AttemptID == "" || d.calls.Load() != 2 || d.prepares.Load() != 2 {
		t.Fatal("original intent did not dispatch exactly once after restart", got, d.calls.Load(), d.prepares.Load())
	}
}

func TestExplicitCLIDraftWaitRefusesMovedHeadAndLeavesOldTerminalHold(t *testing.T) {
	f := supervisorFixture(t)
	old := f.enqueue(t, attemptProject, "old-terminal-draft", "cli", 2, 1)
	a := f.start(t, attemptProject, old)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "pull_draft", ""); err != nil {
		t.Fatal(err)
	}
	request := f.enqueue(t, attemptProject, "new-draft-pinned", "cli", 2, 1)
	now := time.Now()
	f.e.schedulingNow = func() time.Time { return now }
	draft := true
	head := strings.Repeat("a", 40)
	d := &draftWaitDriver{reader: &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: head, BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}}
	drivers := map[string]Driver{"test-capability": d}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	draft = false
	head = strings.Repeat("f", 40)
	now = now.Add(cliDraftProbeInterval)
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	for id, reason := range map[string]string{old.ID: "pull_draft", request.ID: "target_changed"} {
		got, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), id)
		if err != nil || got.State != "held" || got.Reason != reason {
			t.Fatal("terminal hold changed or moved request became eligible", got, err)
		}
	}
	if d.calls.Load() != 0 || d.prepares.Load() != 0 {
		t.Fatal("moved or historical intent reached preparation")
	}
}

func TestExplicitCLIDraftWaitRechecksLocalAuthorityBeforeClaim(t *testing.T) {
	for _, change := range []string{"revoked", "configuration", "redrafted"} {
		t.Run(change, func(t *testing.T) {
			f := supervisorFixture(t)
			now := time.Now()
			f.e.schedulingNow = func() time.Time { return now }
			draft := change != "redrafted"
			d := &draftWaitDriver{reader: &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
				return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
			}}}
			if change == "redrafted" {
				d.prepare = func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
					return Preparation{}, sourceHold("pull_draft")
				}
			}
			drivers := map[string]Driver{"test-capability": d}
			request := f.enqueue(t, attemptProject, "draft-local-guard", "cli", 2, 1)
			process := func() {
				t.Helper()
				if err := f.e.processNext(t.Context(), drivers); err != nil {
					t.Fatal(err)
				}
			}
			read := func() Operation {
				t.Helper()
				o, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), request.ID)
				if err != nil {
					t.Fatal(err)
				}
				return o
			}
			process()
			if got := read(); got.State != "queued" || got.Reason != cliDraftWait || got.AttemptID != "" {
				t.Fatal("draft intent was not retained", got)
			}
			if attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 || d.calls.Load() != 0 {
				t.Fatal("Draft reached dispatch or retained its unused reservation")
			}
			draft = false
			now = now.Add(cliDraftProbeInterval)
			want := "authorization_changed"
			switch change {
			case "revoked":
				if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
					t.Fatal(err)
				}
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
				want = "configuration_changed"
			case "redrafted":
				d.prepare = nil
				process()
				if got := read(); got.State != "completed" || d.calls.Load() != 1 || d.prepares.Load() != 2 {
					t.Fatal("a second readiness transition duplicated or dropped the intent", got)
				}
				return
			}
			process()
			if got := read(); got.State != "held" || got.Reason != want || d.prepares.Load() != 0 {
				t.Fatal("changed local authority did not stop the original request", got)
			}
		})
	}
}

func TestExplicitCLIDraftWaitUsesRealQuotaBaselineOnlyAfterReadiness(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 1, 3, "draft-wait-quota-rounds")
	now := time.Now()
	f.e.schedulingNow = func() time.Time { return now }
	draft := true
	d := &draftWaitDriver{reader: &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}}
	d.prepare = func(ctx context.Context, o Operation, _ Configuration, a Attempt) (Preparation, error) {
		if d.prepares.Load() == 1 {
			return Preparation{}, sourceHold("pull_draft")
		}
		in := CLIAllowanceObservation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 2}
		if err := f.e.RecordCLIExhaustion(ctx, o.ProjectID, a.ID, a.Revision, in); err != nil {
			t.Fatal(err)
		}
		return Preparation{}, sourceHold("allowance_exhausted")
	}
	drivers := map[string]Driver{"test-capability": d}
	request := f.enqueue(t, attemptProject, "draft-wait-before-quota", "cli", 2, 1)
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	if got := cliWaitRead(t, f, request.ID); got.Reason != cliDraftWait || got.Wait != nil || d.prepares.Load() != 0 {
		t.Fatal("Draft wait fabricated allowance state", got)
	}
	draft = false
	now = now.Add(cliDraftProbeInterval)
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	waiting := cliWaitRead(t, f, request.ID)
	if waiting.Reason != cliDraftWait || waiting.AttemptID != "" || waiting.Wait != nil {
		t.Fatal("redraft did not detach before quota", waiting)
	}
	now = now.Add(cliDraftProbeInterval)
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	quota := cliWaitRead(t, f, request.ID)
	cliWaitDue(t, quota)
	if quota.Reason != "" || d.prepares.Load() != 2 || d.calls.Load() != 0 {
		t.Fatal("ready request bypassed genuine quota refusal", quota)
	}
	assertCLIWaitNoCredit(t, f)
	assertCLIWaitSnapshot(t, f)
}

func TestExplicitCLIRedraftProjectsUnusedAttemptAndPreservesRestore(t *testing.T) {
	for _, action := range []string{"ready", "restore", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			now := time.Now()
			f.e.schedulingNow = func() time.Time { return now }
			d := &draftWaitDriver{reader: &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
				draft := false
				return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
			}}}
			var unused Attempt
			d.prepare = func(_ context.Context, _ Operation, _ Configuration, a Attempt) (Preparation, error) {
				unused = a
				return Preparation{}, sourceHold("pull_draft")
			}
			drivers := map[string]Driver{"test-capability": d}
			request := f.enqueue(t, attemptProject, "draft-project-detached", "cli", 2, 1)
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			if got := cliWaitRead(t, f, request.ID); got.State != "queued" || got.Reason != cliDraftWait || got.AttemptID != "" || got.Wait != nil {
				t.Fatal("production projection did not retain queued intent", got)
			}
			_, projected := projectedAttempt(t, f, unused)
			if projected.State != "held" || projected.Reason != cliDraftWait || attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
				t.Fatal("unused preparation did not project or retained its reservation", projected)
			}
			assertCLIWaitSnapshot(t, f)
			control := func(action string) {
				t.Helper()
				op := cliWaitRead(t, f, request.ID)
				if _, err := f.e.ControlOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), "redraft-"+action, op.ID, OperationControl{ExpectedRevision: op.Revision, Action: action}); err != nil {
					t.Fatal(err)
				}
				assertCLIWaitSnapshot(t, f)
			}
			switch action {
			case "restore":
				var summary RestoredWorkSummary
				if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					return abandonRestoredAttempts(ctx, tx, now, &summary)
				}); err != nil {
					t.Fatal(err)
				}
				if got := cliWaitRead(t, f, request.ID); got.State != "held" || got.Reason != "restore_reconciled" || summary.QueuedOperations != 1 || d.calls.Load() != 0 {
					t.Fatal("restore did not settle the queued intent without provider work", got, summary)
				}
			case "cancel":
				control("cancel")
				if got := cliWaitRead(t, f, request.ID); got.State != "cancelled" || d.calls.Load() != 0 {
					t.Fatal("cancel did not settle the queued intent without provider work", got)
				}
			case "ready":
				control("pause")
				control("resume")
				d.prepare = nil
				now = now.Add(cliDraftProbeInterval)
				if err := f.e.processNext(t.Context(), drivers); err != nil {
					t.Fatal(err)
				}
				if got := cliWaitRead(t, f, request.ID); got.State != "completed" || got.AttemptID == unused.ID || d.calls.Load() != 1 {
					t.Fatal("original intent did not dispatch once after redraft", got)
				}
			}
			assertCLIWaitSnapshot(t, f)
		})
	}
}
