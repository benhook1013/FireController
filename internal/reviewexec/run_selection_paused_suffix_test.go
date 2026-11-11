package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRunSelectionStopsBeforePausedHistoricalTail(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			queue := make([]int64, 84)
			for i := range queue {
				queue[i] = int64(i + 1)
			}
			f := schedulingFixtureWithQueue(t, queue)
			for _, number := range queue[56:] {
				schedulingPause(t, f, number, 0, true)
			}
			receipt, _ := selectionEnqueue(t, f, "paused-history", channel, nil)
			r := selectionRead(t, f, receipt.ID)
			c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
			if err != nil {
				t.Fatal(err)
			}
			var calls, donors atomic.Int32
			reader := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				calls.Add(1)
				if n > 56 {
					donors.Add(1)
					<-ctx.Done()
					return reviewprovider.Pull{}, ctx.Err()
				}
				p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1, IsDraft: new(true)}
				if n <= 49 {
					p.State, p.Merged, p.IsDraft = "closed", true, new(false)
				}
				return p, nil
			}}
			choice, err := f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, time.Second)
			if err != nil || choice.state != "held" || choice.reason != "pull_draft" || choice.sourceReason != "" || len(choice.prefix) != 56 || len(choice.pausedSuffix) != 28 || donors.Load() != 0 || calls.Load() > 59 {
				t.Fatal("paused donors consumed source budget or changed hold", choice.state, choice.reason, len(choice.prefix), len(choice.pausedSuffix), calls.Load(), donors.Load(), err)
			}
			t.Logf("84-entry queue: %d raw reads, 56 verified entries, 28 paused donors not fetched", calls.Load())
			selectionNext(t, f, reader)
			frame, ok, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, channel)
			if err != nil || !ok || frame.complete || frame.sourceReason != "" || len(frame.prefix) != 56 || !validFrontFrame(frame, c, channel) {
				t.Fatal("early stop fabricated full source coverage", frame, err)
			}
			held := selectionRead(t, f, receipt.ID)
			if held.State != "held" || held.Reason != "pull_draft" || held.OperationID != "" {
				t.Fatal(held)
			}
		})
	}
}

func TestRunSelectionPausedTailDoesNotHideEligibleSuccessor(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{1, 2, 3})
	schedulingPause(t, f, 2, 0, true)
	receipt, _ := selectionEnqueue(t, f, "unpaused-successor", "cli", nil)
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		p, err := selectionSnapshotReader{}.Pull(ctx, repo, n)
		if n == 1 {
			p.IsDraft = new(true)
		}
		return p, err
	}}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	if got.State != "selected" {
		t.Fatal(got)
	}
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
	if err != nil || op.PullNumber != 3 {
		t.Fatal(op, err)
	}
}

func TestPausedRunSuffixRequiresCurrentPausesAndNoUnseenParents(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{1, 2, 3})
	schedulingPause(t, f, 2, 0, true)
	schedulingPause(t, f, 3, 0, true)
	check := func(prefix []SourcePlan, suffix []int64, want bool) {
		t.Helper()
		err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			got, err := pausedRunSuffix(ctx, tx, attemptProject, prefix, suffix)
			if err == nil && got != want {
				t.Fatalf("paused suffix=%v, want %v", got, want)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check(nil, []int64{2, 3}, true)
	check([]SourcePlan{{ParentPullNumber: 3}}, []int64{2, 3}, false)
	check(nil, []int64{1, 2, 3}, false)
	check(nil, nil, false)
	schedulingPause(t, f, 3, 1, false)
	check(nil, []int64{2, 3}, false)
}

func TestRunSelectionPausedParentStillRequiresVerifiedPrefix(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2, 4})
	revision := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "late-parent", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	schedulingPause(t, f, 4, 0, true)
	reader := &actualParentSelectionReader{pulls: map[int64]reviewprovider.Pull{
		2: {Number: 2, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), HeadRef: "child", BaseSHA: strings.Repeat("c", 40), BaseRef: "parent", ChangedFiles: 1},
		4: {Number: 4, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("c", 40), HeadRef: "parent", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1},
	}}
	receipt, _ := selectionEnqueue(t, f, "paused-parent", "cli", nil)
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	frame, ok, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, "cli")
	if err != nil || !ok || !frame.complete || len(frame.prefix) != 2 || frame.prefix[0].ParentPullNumber != 4 || got.State != "selected" || got.Reason != "" || got.OperationID == "" {
		t.Fatal("paused parent source was skipped or blocked child review", got, frame, err)
	}
}

func TestPausedRunSuffixRejectsMalformedControls(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{1, 2})
	schedulingPause(t, f, 2, 0, true)
	admissionSQL(t, f, `UPDATE review_pull_scheduling SET updated_at='invalid' WHERE pull_number=2`)
	err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		paused, err := pausedRunSuffix(ctx, tx, attemptProject, nil, []int64{2})
		if paused {
			t.Fatal("corrupt pause became scan permission")
		}
		return err
	})
	if err == nil {
		t.Fatal("corrupt pause must fail closed")
	}
}

func TestRunSelectionPausedSuffixPreservesExpectedMismatch(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{1, 2})
	schedulingPause(t, f, 1, 0, true)
	schedulingPause(t, f, 2, 0, true)
	expected := int64(2)
	receipt, _ := selectionEnqueue(t, f, "assert-paused", "cli", &expected)
	selectionNext(t, f, &refreshTestReader{})
	got := selectionRead(t, f, receipt.ID)
	if got.State != "held" || got.Reason != "expected_pr_mismatch" || got.OperationID != "" {
		t.Fatal(got)
	}
	frame, ok, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, "cli")
	if err != nil || !ok || len(frame.prefix) != 1 || frame.complete {
		t.Fatal(frame, err)
	}
}

func TestRunSelectionPausedSuffixCompletionRejectsConcurrentUnpause(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2, 4})
			frontClose(t, f, 2, channel, "close-current-head")
			schedulingPause(t, f, 4, 0, true)
			receipt, _ := selectionEnqueue(t, f, "unpause-before-completion", channel, nil)
			reader, entered, release := selectionContentionReader()
			cancel, done := selectionContentionStart(t, f, reader)
			selectionContentionEntered(t, entered)
			// Hold the writer before releasing source reads. Local selection can read
			// the committed paused suffix, but final persistence must wait behind this
			// transaction, just as in the binding-change contention regression.
			tx := selectionContentionLock(t, f)
			close(release)
			selectionContentionAlive(t, done, 250*time.Millisecond)
			if got := selectionContentionRead(t, f, receipt.ID); got.State != "selecting" || reader.calls.Load() != 2 {
				t.Fatal("selection did not retain its acquired source behind the writer", got, reader.calls.Load())
			}
			if _, err := tx.ExecContext(t.Context(), `UPDATE review_pull_scheduling SET paused=0,reason='',revision=revision+1 WHERE project_id=? AND pull_number=4`, attemptProject); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			got := selectionContentionWait(t, f, receipt.ID, "held")
			selectionContentionStop(t, cancel, done)
			if got.Reason != "selection_changed" || got.OperationID != "" || got.Revision != 3 || reader.calls.Load() != 2 {
				t.Fatal("completion reused stale paused-suffix proof or repeated source", got, reader.calls.Load())
			}
			if _, found, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, channel); err != nil || found {
				t.Fatal("stale paused suffix published a front frame", found, err)
			}
			for _, table := range []string{"review_operations", "review_execution_attempts", "review_front_frames"} {
				if count := attemptCount(t, f.store, `SELECT count(*) FROM `+table); count != 0 {
					t.Fatal("unpause race published source or dispatched work", table, count)
				}
			}
		})
	}
}
