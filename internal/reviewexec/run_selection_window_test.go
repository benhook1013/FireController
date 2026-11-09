package reviewexec

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRunSelectionOpenHeadNeverReadsLookahead(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := selectionFixtureWithQueue(t, []int64{1, 2, 3, 4})
			expected := int64(4)
			receipt, _ := selectionEnqueue(t, f, "no-lookahead", channel, &expected)
			var calls atomic.Int32
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				calls.Add(1)
				if n != 1 {
					t.Errorf("read irrelevant successor %d", n)
				}
				return selectionSnapshotReader{}.Pull(ctx, repo, n)
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, receipt.ID)
			if got.State != "held" || got.Reason != "expected_pr_mismatch" || calls.Load() != 1 || reader.calls.Load() != 2 {
				t.Fatal(got, calls.Load(), reader.calls.Load())
			}
		})
	}
}

func TestRunSelectionLiveTailReadsEachRequiredPullOnce(t *testing.T) {
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
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		calls.Add(1)
		p, err := selectionSnapshotReader{}.Pull(ctx, repo, n)
		p.IsDraft = new(true)
		return p, err
	}}
	choice, err := f.e.chooseRun(t.Context(), r, 801, c, reader)
	if err != nil || choice.state != "held" || choice.reason != "pull_draft" || len(choice.prefix) != 40 || calls.Load() != 40 {
		t.Fatal(choice, calls.Load(), err)
	}
}

func TestRunSelectionClosedUnmergedCanReopen(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2})
	reopened := false
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		p, err := selectionSnapshotReader{}.Pull(ctx, repo, n)
		if n == 1 && !reopened {
			p.State = "closed"
		}
		if n == 2 {
			p.IsDraft = new(true)
		}
		return p, err
	}}
	receipt, _ := selectionEnqueue(t, f, "closed-unmerged", "cli", nil)
	selectionNext(t, f, reader)
	if got := selectionRead(t, f, receipt.ID); got.State != "held" {
		t.Fatal(got)
	}
	reopened = true
	reader.calls.Store(0)
	receipt, _ = selectionEnqueue(t, f, "reopened", "cli", nil)
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
	if err != nil || got.State != "selected" || op.PullNumber != 1 || reader.calls.Load() != 2 {
		t.Fatal(got, op, reader.calls.Load(), err)
	}
}

func TestRunSelectionSourceFailureRetainsOnlyAcquiredPrefix(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2, 3})
	receipt, _ := selectionEnqueue(t, f, "ordered-source-failure", "cli", nil)
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		if n == 2 {
			return reviewprovider.Pull{}, &reviewprovider.Error{Code: "provider_unauthorized", Message: "Private provider response"}
		}
		if n == 3 {
			t.Error("read beyond failure")
		}
		return selectionSnapshotReader{merged: true}.Pull(ctx, repo, n)
	}}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, receipt.ID)
	frame, ok, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, "cli")
	if err != nil || !ok || got.Reason != "source_unauthorized" || frame.complete || len(frame.prefix) != 1 || !frame.prefix[0].Merged || frame.prefix[0].HeadSHA != strings.Repeat("a", 40) || reader.calls.Load() != 3 {
		t.Fatal(got, frame, reader.calls.Load(), err)
	}
}

func TestRunSelectionCancellationStopsCurrentRead(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2, 3})
	receipt, _ := selectionEnqueue(t, f, "cancel-source", "cli", nil)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	var active atomic.Int32
	reader := &refreshTestReader{pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		active.Add(1)
		defer active.Add(-1)
		close(entered)
		<-ctx.Done()
		return reviewprovider.Pull{}, ctx.Err()
	}}
	done := make(chan struct{})
	go func() { defer close(done); f.e.chooseRun(ctx, r, 801, c, reader) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read not joined")
	}
	if active.Load() != 0 || reader.calls.Load() != 2 {
		t.Fatal(active.Load(), reader.calls.Load())
	}
}
