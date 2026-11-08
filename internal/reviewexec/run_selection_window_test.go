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
	automatic, _ := selectionEnqueue(t, f, "long-fresh-prefix-automatic", "cli", nil)
	selectionNext(t, f, reader)
	selected = selectionRead(t, f, automatic.ID)
	op, err = f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), selected.OperationID)
	if err != nil || selected.State != "selected" || op.PullNumber != expected || op.Channel != "cli" || reader.calls.Load() > expected+4 || suffix.Load() > 3 || active.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("automatic selection required an assertion or used lookahead as authority", selected, op, reader.calls.Load(), suffix.Load(), err)
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
	selectionNext(t, f, reader)
	held := selectionRead(t, f, receipt.ID)
	frame, ok, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, "cli")
	if err != nil || !ok || held.State != "held" || held.Reason != "source_unauthorized" || held.OperationID != "" || frame.complete || frame.sourceReason != held.Reason || len(frame.prefix) != 1 || frame.prefix[0].Number != 1 || !frame.prefix[0].Merged || frame.prefix[0].ObservedAt == "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("source failure lost verified prefix, leaked lookahead or created work", held, frame, err)
	}
}

// A held live tail still needs fresh source and real comparison checks. It
// should not repeatedly launch and discard three overlapping successor reads.
func TestRunSelectionLiveTailAvoidsRepeatedSpeculativeReads(t *testing.T) {
	queue := make([]int64, 40)
	for i := range queue {
		queue[i] = int64(i + 1)
	}
	f := selectionFixtureWithQueue(t, queue)
	receipt, _ := selectionEnqueue(t, f, "held-live-tail", "hosted", nil)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	reopened := false
	reader := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		calls.Add(1)
		p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1, IsDraft: new(true)}
		if n <= 8 {
			p.State, p.Merged, p.IsDraft = "closed", true, new(false)
		}
		if reopened && n == 1 {
			p.State, p.Merged, p.IsDraft = "open", false, new(false)
		}
		return p, nil
	}}
	choice, err := f.e.chooseRun(t.Context(), r, 801, c, reader)
	if err != nil || choice.state != "held" || choice.reason != "pull_draft" || len(choice.prefix) != len(queue) || choice.sourceReason != "" {
		t.Fatal("live holds lost full fresh ordered coverage", choice, err)
	}
	t.Logf("raw pull reads: %d for %d verified PRs", calls.Load(), len(choice.prefix))
	// Eight merged reads, one four-wide transition, then single live reads.
	if got := calls.Load(); got != int32(len(queue)+3) {
		t.Fatalf("raw pull reads=%d, want %d", got, len(queue)+3)
	}
	for i, p := range choice.prefix {
		if p.Pull.Number != queue[i] || p.ObservedAt == "" || (i >= 8 && p.SourceReason != "pull_draft") {
			t.Fatal("unverified or reordered prefix", i, p)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("selection manufactured execution")
	}

	// Width is phase-local, not a stored lifecycle claim: a formerly merged
	// predecessor reopening must win freshly, ahead of the previously held tail.
	reopened = true
	calls.Store(0)
	choice, err = f.e.chooseRun(t.Context(), r, 801, c, reader)
	if err != nil || choice.state != "selected" || choice.number != 1 || len(choice.prefix) != 1 || choice.prefix[0].Pull.Merged || calls.Load() != 4 {
		t.Fatal("freshly reopened predecessor did not regain priority", choice, calls.Load(), err)
	}
}
