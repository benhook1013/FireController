package reviewexec

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRunSelectionFreshWindowsReachLongMergedPrefix(t *testing.T) {
	queue := make([]int64, 40)
	for i := range queue {
		queue[i] = int64(i + 1)
	}
	f := selectionFixtureWithQueue(t, queue)
	expected := int64(34)
	receipt, _ := selectionEnqueue(t, f, "long-fresh-prefix", "hosted", &expected)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	var active, maximum, suffix atomic.Int32
	reader := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		if n > expected {
			suffix.Add(1)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return reviewprovider.Pull{}, ctx.Err()
		case <-timer.C:
		}
		p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
		if n < expected {
			p.State, p.Merged = "closed", true
		}
		return p, nil
	}}
	// The production budget stays fifteen seconds. Scale only this private
	// phase's budget: sequential acquisition needs850ms, four fresh slots225ms.
	choice, err := f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, 500*time.Millisecond)
	if err != nil || choice.state != "selected" || choice.number != expected || len(choice.prefix) != int(expected) {
		t.Fatal("long merged prefix exhausted selection before the live target", choice.state, choice.reason, choice.number, len(choice.prefix), err)
	}
	if maximum.Load() < 2 || maximum.Load() > 4 || suffix.Load() != 0 || active.Load() != 0 || reader.calls.Load() != expected+1 {
		t.Fatal("source window exceeded its bound, assertion or phase", maximum.Load(), suffix.Load(), active.Load(), reader.calls.Load())
	}
	reader.calls.Store(0)
	selectionNext(t, f, reader)
	selected := selectionRead(t, f, receipt.ID)
	if selected.State != "selected" || selected.OperationID == "" || reader.calls.Load() != expected+1 || suffix.Load() != 0 || active.Load() != 0 {
		t.Fatal("ordinary worker did not link exactly the asserted original target", selected, reader.calls.Load(), active.Load())
	}
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), selected.OperationID)
	if err != nil || op.PullNumber != expected || op.Channel != "hosted" || op.HeadSHA != strings.Repeat("a", 40) || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("source selection started work or changed the child binding", op, err)
	}
	// Omitting the optional assertion uses the same fast fresh windows. Its
	// discarded lookahead supplies neither child identity nor review credit.
	reader.calls.Store(0)
	explicit, _ := selectionEnqueue(t, f, "long-fresh-prefix-explicit", "cli", nil)
	selectionNext(t, f, reader)
	selected = selectionRead(t, f, explicit.ID)
	op, err = f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), selected.OperationID)
	if err != nil || selected.State != "selected" || op.PullNumber != expected || op.Channel != "cli" || reader.calls.Load() > expected+4 || suffix.Load() > 3 || active.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("explicit selection required an assertion or used lookahead as authority", selected, op, reader.calls.Load(), suffix.Load(), err)
	}
}

func TestRunSelectionFreshWindowCancellationJoinsAllReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{}, 4)
	var active atomic.Int32
	reader := &refreshTestReader{pull: func(ctx context.Context, _ string, _ int64) (reviewprovider.Pull, error) {
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		<-ctx.Done()
		return reviewprovider.Pull{}, ctx.Err()
	}}
	reads := &runReads{reader: reader, pulls: map[string]map[int64]reviewprovider.Pull{}}
	budget := runSourceBudget{remaining: time.Second}
	done := make(chan error, 1)
	go func() {
		done <- budget.read(ctx, func(source context.Context) error {
			reads.pullWindow(source, "owner/repo", 801, []int64{1, 2, 3, 4})
			return nil
		})
	}()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("metadata window did not start its bounded readers")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || active.Load() != 0 || len(reads.pulls["owner/repo"]) != 0 {
			t.Fatal("canceled window retained source or leaked readers", err, active.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("metadata readers did not join on cancellation")
	}
}

func TestRunSelectionEarlyMismatchCancelsLookaheadAndFailureRetainsPrefix(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2, 3})
	expected := int64(3)
	receipt, _ := selectionEnqueue(t, f, "early-window-mismatch", "cli", &expected)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	blocked, failed := make(chan struct{}), make(chan struct{})
	var joined atomic.Bool
	reader := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		switch n {
		case 1:
			<-blocked
			<-failed
			return selectionSnapshotReader{}.Pull(ctx, "owner/repo", n)
		case 2:
			close(blocked)
			<-ctx.Done()
			joined.Store(true)
			return reviewprovider.Pull{}, ctx.Err()
		default:
			close(failed)
			return reviewprovider.Pull{}, &reviewprovider.Error{Code: "provider_unauthorized", Message: "Private provider response"}
		}
	}}
	started := time.Now()
	choice, err := f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, 500*time.Millisecond)
	if err != nil || choice.state != "held" || choice.reason != "expected_pr_mismatch" || len(choice.prefix) != 1 || !joined.Load() || time.Since(started) >= 500*time.Millisecond {
		t.Fatal("unused slow/failed lookahead overrode the earlier fresh candidate", choice.state, choice.reason, len(choice.prefix), joined.Load(), time.Since(started), err)
	}
	// The next real worker phase uses a fresh merged first member, then a
	// required provider failure. Only that ordered, verified prefix is retained.
	reader = &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		if n == 2 {
			return reviewprovider.Pull{}, &reviewprovider.Error{Code: "provider_unauthorized", Message: "Private provider response"}
		}
		return selectionSnapshotReader{merged: true}.Pull(ctx, "owner/repo", n)
	}}
	choice, err = f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	selectionNext(t, f, reader)
	held := selectionRead(t, f, receipt.ID)
	if err != nil || held.State != "held" || held.Reason != "source_unauthorized" || held.OperationID != "" || choice.reason != held.Reason || len(choice.prefix) != 1 || choice.prefix[0].Pull.Number != 1 || !choice.prefix[0].Pull.Merged || choice.prefix[0].ObservedAt == "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("source failure lost verified prefix, leaked lookahead or created work", held, choice, err)
	}
}

func TestRunSelectionInjectedBudgetBoundsProviderReads(t *testing.T) {
	f := selectionFixture(t)
	f.e.runSourceBudget = 30 * time.Millisecond
	receipt, _ := selectionEnqueue(t, f, "injected-source-deadline", "cli", nil)
	reader := &refreshTestReader{repository: func(ctx context.Context, _ int64) (reviewprovider.Repository, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > f.e.runSourceBudget {
			t.Error("provider did not receive injected source deadline")
		}
		<-ctx.Done()
		return reviewprovider.Repository{}, ctx.Err()
	}}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	if got.State != "held" || got.Reason != "source_budget_exhausted" || got.OperationID != "" || reader.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || t.Context().Err() != nil {
		t.Fatal("provider timeout changed local authority or canceled worker", got, reader.calls.Load())
	}
}

func TestRunSelectionFreshWindowStopsAtPausedSuffix(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{1, 2, 3, 4})
	for _, n := range []int64{2, 3, 4} {
		schedulingPause(t, f, n, 0, true)
	}
	expected := int64(4)
	receipt, _ := selectionEnqueue(t, f, "fresh-paused-barrier", "cli", &expected)
	var calls atomic.Int32
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		calls.Add(1)
		if n != 1 {
			t.Errorf("paused speculative source read: %d", n)
		}
		return selectionSnapshotReader{}.Pull(ctx, repo, n)
	}}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	if got.State != "held" || got.Reason != "expected_pr_mismatch" || got.OperationID != "" || calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("paused barrier changed selection or acquired source", got, calls.Load())
	}
}

// Phase-level parent refresh: retained merged metadata is not proof of a
// current actual parent when a newly acquired child names it.
func TestRunSelectionFreshWindowRefreshesRetainedActualParent(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{2, 4})
	reader := &actualParentSelectionReader{pulls: map[int64]reviewprovider.Pull{
		2: {Number: 2, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), HeadRef: "parent", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1},
	}}
	reads := &runReads{reader: reader, repos: map[int64]reviewprovider.Repository{}, pulls: map[string]map[int64]reviewprovider.Pull{}}
	prefix := []SourcePlan{
		{Pull: reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "closed", Merged: true, HeadSHA: strings.Repeat("c", 40)}, retainedMerged: true},
		{Pull: reviewprovider.Pull{Number: 4, RepositoryID: 801}, ParentPullNumber: 2, ParentSHA: strings.Repeat("a", 40)},
	}
	budget := runSourceBudget{remaining: time.Second}
	c := Configuration{Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}
	if err := f.e.resolveRunParents(t.Context(), &budget, reads, 801, c, "cli", prefix); err != nil {
		t.Fatal(err)
	}
	if prefix[0].retainedMerged || prefix[0].Pull.Merged || prefix[0].Pull.HeadSHA != strings.Repeat("a", 40) || prefix[0].ObservedAt == "" {
		t.Fatal("retained parent bypassed fresh identity", prefix[0])
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.calls) != 2 || reader.calls[0] != "repository:801" || reader.calls[1] != "pull:2" {
		t.Fatal("parent refresh lost bounded identity", reader.calls)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("parent refresh authorized a child")
	}
}
