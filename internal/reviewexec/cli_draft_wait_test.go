package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type draftWaitDriver struct {
	controlledDriver
	reader TargetReader
}

func (d *draftWaitDriver) SourceReader() TargetReader { return d.reader }

func markCLIDraftWait(t *testing.T, f *attemptFixture, operationID string) {
	t.Helper()
	o := cliWaitRead(t, f, operationID)
	if err := f.e.recordCLIDraftDecision(t.Context(), o, cliDraftWait); err != nil {
		t.Fatal(err)
	}
}

func draftProbeClaimed(e *Engine, operationID string) bool {
	e.draftProbeMu.Lock()
	defer e.draftProbeMu.Unlock()
	_, claimed := e.draftProbeNext[operationID]
	return claimed
}

func TestSlowCLIDraftProbesLeaveOtherProjectSchedulable(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	seed := f.enqueue(t, attemptProject, "slow-draft-prefix", "cli", 2, 1)
	for i := range 31 {
		schedulerQueuedCopy(t, f, seed.ID, fmt.Sprintf("review_operation_slowdraft%03d", i))
	}
	admissionSQL(t, f, `UPDATE review_operations SET reason=? WHERE project_id=?`, cliDraftWait, attemptProject)
	entered := make(chan struct{}, 2)
	var active, peak atomic.Int32
	reader := &refreshTestReader{repository: func(ctx context.Context, _ int64) (reviewprovider.Repository, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
			t.Error("draft probe has no finite provider deadline")
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		<-ctx.Done()
		return reviewprovider.Repository{}, ctx.Err()
	}}
	d := &draftWaitDriver{reader: reader}
	stop := runSupervisor(t, f, d)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("draft probe never started")
	}
	healthy := f.enqueue(t, otherAttemptProject, "ready-during-slow-probe", "cli", 2, 1)
	waitOperationState(t, f, healthy.ID, "completed")
	if d.prepares.Load() != 1 || d.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts WHERE project_id=?`, attemptProject) != 0 {
		t.Fatal("draft probe consumed an attempt or ready request dispatched twice")
	}
	stop()
	if peak.Load() != 1 || active.Load() != 0 {
		t.Fatal("both dispatch workers probed Drafts or shutdown leaked a probe", peak.Load(), active.Load())
	}
}

func TestCLIDraftProbePassIsBoundedAndRotatesAfterTimeout(t *testing.T) {
	f := supervisorFixture(t)
	seed := f.enqueue(t, attemptProject, "rotating-draft-probes", "cli", 2, 1)
	for i := range 2 {
		schedulerQueuedCopy(t, f, seed.ID, fmt.Sprintf("review_operation_rotate%03d", i))
	}
	admissionSQL(t, f, `UPDATE review_operations SET reason=?`, cliDraftWait)
	now := time.Now()
	f.e.schedulingNow = func() time.Time { return now }
	reader := &refreshTestReader{repository: func(ctx context.Context, _ int64) (reviewprovider.Repository, error) {
		short, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		<-short.Done()
		return reviewprovider.Repository{}, short.Err()
	}}
	d := &draftWaitDriver{reader: reader}
	seen := map[string]bool{}
	for pass := range 3 {
		before := reader.calls.Load()
		if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
			t.Fatal(err)
		}
		if reader.calls.Load()-before != 1 {
			t.Fatal("one scheduling pass performed multiple Draft probes", pass, reader.calls.Load()-before)
		}
		f.e.draftProbeMu.Lock()
		for id, next := range f.e.draftProbeNext {
			if next.After(now) {
				if seen[id] {
					t.Error("expired early probe starved a later wait", id)
				}
				seen[id] = true
			}
		}
		f.e.draftProbeMu.Unlock()
		now = now.Add(cliDraftProbeInterval)
	}
	if len(seen) != 3 || d.prepares.Load() != 0 || d.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("probe rotation skipped a wait or acquired review capacity", seen)
	}
}

func TestSupervisorNonDraftSelectionSkipsDraftPrefix(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	seed := f.enqueue(t, attemptProject, "draft-candidate-prefix", "cli", 2, 1)
	healthy := f.enqueue(t, otherAttemptProject, "ready-after-draft-prefix", "cli", 2, 1)
	// Legacy imported rows can exceed ordinary admission's smaller capacity.
	// The non-probing worker must exclude them before its 256-row window.
	for i := range 299 {
		schedulerQueuedCopy(t, f, seed.ID, fmt.Sprintf("review_operation_prefix%03d", i))
	}
	admissionSQL(t, f, `UPDATE review_operations SET reason=? WHERE project_id=?`, cliDraftWait, attemptProject)
	operations, err := f.e.pendingForWorker(t.Context(), false)
	if err != nil || len(operations) != 1 || operations[0].ID != healthy.ID {
		t.Fatal("draft prefix hid independent ready operation", len(operations), err)
	}
	d := &draftWaitDriver{reader: &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		t.Error("non-probing worker attempted a Draft provider read")
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}}}
	if err := f.e.processNextForWorker(t.Context(), map[string]Driver{"test-capability": d, "other-account": d}, false); err != nil {
		t.Fatal(err)
	}
	if supervisorOperationState(t, f, healthy.ID) != "completed" || d.prepares.Load() != 1 || d.calls.Load() != 1 {
		t.Fatal("independent ready work did not dispatch exactly once")
	}
}

func TestSupervisorProbesDraftBeyondOrdinaryCandidateWindow(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	seed := f.enqueue(t, attemptProject, "ordinary-candidate-prefix", "cli", 2, 1)
	draft := f.enqueue(t, otherAttemptProject, "draft-after-prefix", "cli", 2, 1)
	for i := range 299 {
		schedulerQueuedCopy(t, f, seed.ID, fmt.Sprintf("review_operation_ordinary%03d", i))
	}
	admissionSQL(t, f, `UPDATE review_operations SET reason=? WHERE id=?`, cliDraftWait, draft.ID)
	operations, err := f.e.pendingForWorker(t.Context(), true)
	if err != nil || len(operations) < 2 || operations[0].ID != draft.ID {
		t.Fatalf("ordinary window hid its independent Draft probe: %d candidates, %v", len(operations), err)
	}
}

func TestOrdinaryCLIRequestUsesPreparationWithoutDraftProbe(t *testing.T) {
	f := supervisorFixture(t)
	request := f.enqueue(t, attemptProject, "ordinary-cli-no-draft-probe", "cli", 2, 1)
	reader := &refreshTestReader{}
	d := &draftWaitDriver{reader: reader}
	d.prepare = func(ctx context.Context, o Operation, c Configuration, _ Attempt) (Preparation, error) {
		_, err := ResolveSource(ctx, reader, 801, c, Request{PullNumber: o.PullNumber, Channel: o.Channel, HeadSHA: o.HeadSHA})
		if err != nil {
			return Preparation{}, err
		}
		return Preparation{Anchor: attemptAnchor(o.ProjectID, o.PullNumber)}, nil
	}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
		t.Fatal(err)
	}
	if got := cliWaitRead(t, f, request.ID); got.State != "completed" || d.prepares.Load() != 1 || d.calls.Load() != 1 {
		t.Fatal("ordinary CLI request did not use normal preparation", got, d.prepares.Load(), d.calls.Load())
	}
	if reader.calls.Load() != 2 {
		t.Fatalf("ordinary request made %d provider reads, want only the repository and pull reads from preparation", reader.calls.Load())
	}
	if draftProbeClaimed(f.e, request.ID) {
		t.Fatal("ordinary preparation consumed a draft-probe slot")
	}
}

func TestOrdinaryCLIPreparationRetainsFirstDraftDiscovery(t *testing.T) {
	f := supervisorFixture(t)
	request := f.enqueue(t, attemptProject, "ordinary-cli-discovers-draft", "cli", 2, 1)
	draft := true
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	d := &draftWaitDriver{reader: reader}
	d.prepare = func(ctx context.Context, o Operation, c Configuration, _ Attempt) (Preparation, error) {
		_, err := ResolveSource(ctx, reader, 801, c, Request{PullNumber: o.PullNumber, Channel: o.Channel, HeadSHA: o.HeadSHA})
		return Preparation{}, err
	}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
		t.Fatal(err)
	}
	if got := cliWaitRead(t, f, request.ID); got.State != "queued" || got.Reason != cliDraftWait || got.AttemptID != "" || d.prepares.Load() != 1 || d.calls.Load() != 0 {
		t.Fatal("first draft discovery did not retain the explicit request through preparation", got, d.prepares.Load(), d.calls.Load())
	}
	if reader.calls.Load() != 2 || draftProbeClaimed(f.e, request.ID) {
		t.Fatalf("first discovery used %d source reads or consumed a wait-probe slot", reader.calls.Load())
	}
	if attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
		t.Fatal("draft discovered during preparation retained an unused reservation")
	}
}

func TestSourceLessOrdinaryCLIDraftRefusalIsNotRetainedAsWait(t *testing.T) {
	f := supervisorFixture(t)
	request := f.enqueue(t, attemptProject, "ordinary-cli-source-less-draft", "cli", 2, 1)
	d := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, sourceHold("pull_draft")
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
		t.Fatal(err)
	}
	if got := cliWaitRead(t, f, request.ID); got.State != "held" || got.Reason != "pull_draft" || got.Reason == cliDraftWait || d.prepares.Load() != 1 || d.calls.Load() != 0 {
		t.Fatal("source-less preparation gained an unprobeable retained Draft wait", got, d.prepares.Load(), d.calls.Load())
	}
}

func TestExplicitCLIDraftWaitReusesIntentAndLeavesIndependentWorkEligible(t *testing.T) {
	f := supervisorFixture(t)
	now := time.Now()
	f.e.schedulingNow = func() time.Time { return now }
	draft := true
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		isDraft := number == 2 && draft
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &isDraft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	d := &draftWaitDriver{reader: reader}
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
	markCLIDraftWait(t, f, request.ID)
	run()
	waiting := read(request.ID)
	if waiting.State != "queued" || waiting.Reason != cliDraftWait || waiting.AttemptID != "" || waiting.Wait != nil || d.prepares.Load() != 0 || reader.calls.Load() != 2 || !draftProbeClaimed(f.e, request.ID) || attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
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
	markCLIDraftWait(t, f, request.ID)
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
	for id, reason := range map[string]string{old.ID: "pull_draft", request.ID: "target_superseded"} {
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
			if change != "redrafted" {
				markCLIDraftWait(t, f, request.ID)
			}
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
	markCLIDraftWait(t, f, request.ID)
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

func TestCLIDraftProbeThrottleOnlyBlocksWaitingRequests(t *testing.T) {
	for _, cause := range []string{"recent", "capacity"} {
		for _, waiting := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/waiting=%t", cause, waiting), func(t *testing.T) {
				f := supervisorFixture(t)
				now := time.Now()
				f.e.schedulingNow = func() time.Time { return now }
				request := f.enqueue(t, attemptProject, "probe-throttled", "cli", 2, 1)
				if waiting {
					o, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.e.recordCLIDraftDecision(t.Context(), o, cliDraftWait); err != nil {
						t.Fatal(err)
					}
				}
				f.e.draftProbeNext = map[string]time.Time{}
				if cause == "recent" {
					f.e.draftProbeNext[request.ID] = now.Add(cliDraftProbeInterval)
				} else {
					for i := range cliDraftProbeCapacity {
						f.e.draftProbeNext[fmt.Sprintf("other-%d", i)] = now.Add(cliDraftProbeInterval)
					}
				}
				d := &draftWaitDriver{reader: &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
					t.Error("throttled probe still read the provider")
					return reviewprovider.Pull{}, nil
				}}}
				if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
					t.Fatal(err)
				}
				o, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
				if err != nil {
					t.Fatal(err)
				}
				if waiting {
					if o.State != "queued" || o.Reason != cliDraftWait || o.AttemptID != "" || d.prepares.Load() != 0 {
						t.Fatal("waiting request bypassed probe throttle", o)
					}
				} else if o.State != "completed" || d.prepares.Load() != 1 || d.calls.Load() != 1 {
					t.Fatal("ordinary request was delayed by draft probe throttle", o, d.prepares.Load(), d.calls.Load())
				}
			})
		}
	}
}

func TestCLIDraftAfterThrottledProbeRetainsExplicitIntent(t *testing.T) {
	for _, cause := range []string{"recent", "capacity"} {
		t.Run(cause, func(t *testing.T) {
			f := supervisorFixture(t)
			now := time.Now()
			f.e.schedulingNow = func() time.Time { return now }
			r := f.enqueue(t, attemptProject, "throttled-then-draft", "cli", 2, 1)
			markCLIDraftWait(t, f, r.ID)
			f.e.draftProbeNext = map[string]time.Time{}
			if cause == "recent" {
				f.e.draftProbeNext[r.ID] = now.Add(cliDraftProbeInterval)
			} else {
				for i := range cliDraftProbeCapacity {
					f.e.draftProbeNext[fmt.Sprintf("occupied-%d", i)] = now.Add(cliDraftProbeInterval)
				}
			}
			d := &draftWaitDriver{reader: &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
				draft := false
				return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
			}}}
			d.prepare = func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
				return Preparation{}, sourceHold("pull_draft")
			}
			drivers := map[string]Driver{"test-capability": d}
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			throttled := cliWaitRead(t, f, r.ID)
			if throttled.State != "queued" || throttled.Reason != cliDraftWait || throttled.AttemptID != "" || d.prepares.Load() != 0 || d.calls.Load() != 0 {
				t.Fatal("throttled wait changed the explicit intent or reached preparation", throttled)
			}
			now = now.Add(cliDraftProbeInterval)
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			o, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), r.ID)
			if err != nil || o.State != "queued" || o.Reason != cliDraftWait || o.AttemptID != "" || d.calls.Load() != 0 || d.prepares.Load() != 1 || attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
				t.Fatal("throttled probe lost explicit intent after Draft refusal", o, err)
			}
			now = now.Add(cliDraftProbeInterval)
			d.prepare = nil
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			o, err = f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), r.ID)
			if err != nil || o.ID != r.ID || o.State != "completed" || d.calls.Load() != 1 || d.prepares.Load() != 2 {
				t.Fatal("retained intent did not resume once after readiness", o, err)
			}
		})
	}
}

func TestCLIDraftWaitMissingDriverSettlesOriginalIntent(t *testing.T) {
	for _, kind := range []string{"missing", "nil", "without-source-interface", "nil-source-reader"} {
		t.Run(kind, func(t *testing.T) {
			f := supervisorFixture(t)
			now := time.Now()
			f.e.schedulingNow = func() time.Time { return now }
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			defer slog.SetDefault(previous)
			request := f.enqueue(t, attemptProject, "draft-driver-original", "cli", 2, 1)
			original := cliWaitRead(t, f, request.ID)
			if err := f.e.recordCLIDraftDecision(t.Context(), original, cliDraftWait); err != nil {
				t.Fatal(err)
			}
			drivers := map[string]Driver{}
			switch kind {
			case "nil":
				drivers["test-capability"] = nil
			case "without-source-interface":
				drivers["test-capability"] = &controlledDriver{}
			case "nil-source-reader":
				drivers["test-capability"] = &draftWaitDriver{}
			}
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			got := cliWaitRead(t, f, request.ID)
			missing := kind == "missing" || kind == "nil"
			if missing {
				if got.State != "held" || got.Reason != "capability_unavailable" || got.AttemptID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
					t.Fatal("missing driver stranded Draft intent", got)
				}
				// The terminal original no longer blocks another authorized exact request.
				if next := f.enqueue(t, attemptProject, "draft-driver-next", "cli", 2, 1); next.ID == request.ID {
					t.Fatal("new key adopted old intent")
				}
			} else if got.State != "queued" || got.Reason != cliDraftWait || got.AttemptID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
				t.Fatal("registered source-less driver changed Draft wait", got)
			}
			if !missing {
				for range 2 {
					if err := f.e.processNext(t.Context(), drivers); err != nil {
						t.Fatal(err)
					}
				}
				text := output.String()
				if strings.Count(text, "CLI draft source probe failed") != 1 || !strings.Contains(text, request.ID) || !strings.Contains(text, `"reason":"source_reader_unavailable"`) || !strings.Contains(text, `"provider_operation":"source_reader"`) {
					t.Fatal("source-less wait diagnostic was silent or unbounded", text)
				}
				now = now.Add(cliDraftProbeInterval)
				if err := f.e.processNext(t.Context(), drivers); err != nil {
					t.Fatal(err)
				}
				if strings.Count(output.String(), "CLI draft source probe failed") != 2 {
					t.Fatal("next bounded source-less diagnostic was not observable")
				}
				after := cliWaitRead(t, f, request.ID)
				if after.ID != got.ID || after.Revision != got.Revision || after.State != got.State || after.Reason != got.Reason || after.AttemptID != "" {
					t.Fatal("source-less diagnostics changed retained intent", after)
				}
			}
			if replay := f.enqueue(t, attemptProject, "draft-driver-original", "cli", 2, 1); replay.ID != request.ID {
				t.Fatal("original request identity lost")
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("missing or source-less driver retained capacity")
			}
		})
	}
}

func TestCLIDraftProbeReadFailuresAreBoundedAndSafe(t *testing.T) {
	for _, stage := range []string{"repository", "pull", "unknown-draft"} {
		t.Run(stage, func(t *testing.T) {
			f := supervisorFixture(t)
			now := time.Now()
			f.e.schedulingNow = func() time.Time { return now }
			r := f.enqueue(t, attemptProject, "draft-probe-failure", "cli", 2, 1)
			markCLIDraftWait(t, f, r.ID)
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			defer slog.SetDefault(previous)
			reader := &refreshTestReader{}
			failure := &reviewprovider.Error{Code: "provider_unavailable", Message: "PRIVATE_PROVIDER_BODY"}
			if stage == "repository" {
				reader.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
					return reviewprovider.Repository{}, failure
				}
			} else if stage == "pull" {
				reader.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
					return reviewprovider.Pull{}, failure
				}
			} else {
				reader.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
					p := lifecyclePull(2)
					p.IsDraft = nil
					return p, nil
				}
			}
			d := &draftWaitDriver{reader: reader}
			drivers := map[string]Driver{"test-capability": d}
			for range 2 {
				if err := f.e.processNext(t.Context(), drivers); err != nil {
					t.Fatal(err)
				}
			}
			text := output.String()
			providerStage := stage
			if stage == "unknown-draft" {
				providerStage = "pull"
				if !strings.Contains(text, `"reason":"draft_state_unknown"`) {
					t.Fatal("missing unknown Draft reason", text)
				}
			}
			if strings.Count(text, "CLI draft source probe failed") != 1 || !strings.Contains(text, r.ID) || !strings.Contains(text, `"provider_operation":"`+providerStage+`"`) || strings.Contains(text, "PRIVATE_PROVIDER_BODY") {
				t.Fatal("probe failure was silent, unbounded or exposed a provider body", text)
			}
			now = now.Add(cliDraftProbeInterval)
			if err := f.e.processNext(t.Context(), drivers); err != nil {
				t.Fatal(err)
			}
			if strings.Count(output.String(), "CLI draft source probe failed") != 2 {
				t.Fatal("next bounded probe failure was not observable")
			}
			if got := cliWaitRead(t, f, r.ID); got.State != "queued" || got.Reason != cliDraftWait || got.AttemptID != "" || d.prepares.Load() != 0 || d.calls.Load() != 0 {
				t.Fatal("provider failure changed intent or reached provider review", got)
			}
		})
	}
}

type draftHistoryQuery struct {
	EvidenceQuery
	second string
	calls  int
}

func (q *draftHistoryQuery) QueryRowContext(ctx context.Context, _ string, _ ...any) *sql.Row {
	q.calls++
	if q.calls == 1 {
		return q.EvidenceQuery.QueryRowContext(ctx, `SELECT 'cli_draft_wait'`)
	}
	return q.EvidenceQuery.QueryRowContext(ctx, q.second)
}

func TestHistoricalCLIDraftSeparatesLookupFailureFromInvalidHistory(t *testing.T) {
	f := supervisorFixture(t)
	o := Operation{ID: "operation", ProjectID: attemptProject, Channel: "cli"}
	a := Attempt{ID: "attempt", OperationID: o.ID, ProjectID: o.ProjectID, State: "held", Revision: 2}
	for _, tc := range []struct {
		name, query     string
		noRows, invalid bool
	}{
		{"missing", `SELECT 1 WHERE 0`, true, false},
		{"storage", `SELECT * FROM nonexistent_draft_history`, false, false},
		{"invalid-time", `SELECT 'invalid', 'invalid', 0`, false, true},
		{"occupied", `SELECT '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 1`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &draftHistoryQuery{EvidenceQuery: f.store.DB(), second: tc.query}
			_, draft, err := historicalCLIDraftOperation(t.Context(), q, o, a)
			if draft || err == nil {
				t.Fatal("invalid lookup became retained history", draft, err)
			}
			if tc.noRows && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("missing-row identity was lost", err)
			}
			if tc.invalid && err.Error() != waitUnavailable().Error() {
				t.Fatal("invalid history lost its classification", err)
			}
			if tc.name == "storage" && !strings.Contains(err.Error(), "nonexistent_draft_history") {
				t.Fatal("storage error was rewritten as invalid history", err)
			}
		})
	}
}

func TestCLIDraftRawPageAdvancesPastIneligiblePrefix(t *testing.T) {
	f := supervisorFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	seed := f.enqueue(t, attemptProject, "draft-page-blocked", "cli", 2, 1)
	ready := f.enqueue(t, otherAttemptProject, "draft-page-ready", "cli", 2, 1)
	for i := range cliDraftProbeCapacity + 1 {
		schedulerQueuedCopy(t, f, seed.ID, fmt.Sprintf("review_operation_paging%03d", i))
	}
	const target = "review_operation_paging999"
	schedulerQueuedCopy(t, f, ready.ID, target)
	admissionSQL(t, f, `UPDATE review_operations SET reason=?`, cliDraftWait)
	admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.paused',1) WHERE project_id=?`, attemptProject)
	f.e.draftProbeCursor = "review_operation_paging"
	operations, err := f.e.pending(t.Context())
	if err != nil || len(operations) != 0 {
		t.Fatal("ineligible raw page escaped its bound", operations, err)
	}
	if got := f.e.draftProbeCursor; got != "review_operation_paging256" {
		t.Fatal("empty eligible page did not advance raw cursor", got)
	}
	operations, err = f.e.pending(t.Context())
	if err != nil || len(operations) != 1 || operations[0].ID != target {
		t.Fatal("next raw page did not reach ready Draft", operations, err)
	}
	if len(f.e.draftProbeNext) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("raw paging acquired provider capacity")
	}
	// The bounded seek uses the partial Draft index, not a whole-queue sort.
	rows, err := f.store.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT project_id,id FROM review_operations INDEXED BY review_operations_draft_probe WHERE state='queued' AND channel='cli' AND reason='cli_draft_wait' AND id>? ORDER BY id LIMIT ?`, "review_operation_paging", cliDraftProbeCapacity+1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "SEARCH review_operations USING INDEX review_operations_draft_probe (id>?)") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("Draft seek is not bounded by the raw index", plan)
	}
}
