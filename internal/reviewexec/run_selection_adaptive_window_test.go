package reviewexec

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Exercise the real selector with no retained open hints. Drafts are evaluated
// normally, not skipped by the window policy.
func TestRunSelectionAdaptiveUnknownOpenQueue(t *testing.T) {
	for _, mode := range []string{"hosted/unknown", "cli/unknown", "hosted/known", "cli/known"} {
		t.Run(mode, func(t *testing.T) {
			queue := make([]int64, 17)
			for i := range queue {
				queue[i] = int64(i + 1)
			}
			channel := strings.Split(mode, "/")[0]
			var f *attemptFixture
			if strings.HasSuffix(mode, "/known") {
				f = selectionHistoryFixture(t, queue, 0)
			} else {
				f = selectionFixtureWithQueue(t, queue)
			}
			receipt, _ := selectionEnqueue(t, f, "adaptive-open", channel, nil)
			r := selectionRead(t, f, receipt.ID)
			c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
			if err != nil {
				t.Fatal(err)
			}
			var calls, cancelled atomic.Int32
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				calls.Add(1)
				select {
				case <-ctx.Done():
					cancelled.Add(1)
					return reviewprovider.Pull{}, ctx.Err()
				case <-time.After(2 * time.Millisecond):
				}
				p, err := selectionSnapshotReader{}.Pull(ctx, repo, n)
				if n < 17 {
					p.IsDraft = new(true)
				}
				return p, err
			}}
			start := time.Now()
			choice, err := f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, time.Second)
			if err != nil || choice.state != "selected" || choice.number != 17 || len(choice.prefix) != 17 || calls.Load() != 17 || cancelled.Load() != 0 || reader.calls.Load() != 18 {
				t.Fatal(choice.state, choice.reason, choice.number, len(choice.prefix), calls.Load(), cancelled.Load(), reader.calls.Load(), err)
			}
			t.Logf("17 open entries (%s): metadata=%d repository=1 cancelled=%d elapsed=%s", mode, calls.Load(), cancelled.Load(), time.Since(start))
		})
	}
}

// Compare the unchanged raw window primitive at fixed and adaptive widths.
// The head completes in 3ms; speculative live suffixes take 30ms and are
// cancelled. Merged runs have uniform latency. This is synthetic metadata
// latency, not GitHub charged quota or a production latency guarantee.
func TestRunSelectionAdaptiveWindowCost(t *testing.T) {
	for _, scenario := range []string{"open", "merged", "mixed"} {
		for _, adaptive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/adaptive=%t", scenario, adaptive), func(t *testing.T) {
				var calls, cancelled, head atomic.Int32
				merged := func(n int64) bool {
					return scenario == "merged" || scenario == "mixed" && (n <= 8 || n >= 13 && n <= 20)
				}
				reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
					calls.Add(1)
					delay := 3 * time.Millisecond
					// All-open: expose cancellation waste with slow suffix calls.
					// Mixed: use fixed slow identities, independent of window placement.
					if scenario == "open" && n > int64(head.Load()) || scenario == "mixed" && (n == 9 || n == 21) {
						delay = 30 * time.Millisecond
					}
					select {
					case <-ctx.Done():
						cancelled.Add(1)
						return reviewprovider.Pull{}, ctx.Err()
					case <-time.After(delay):
					}
					return selectionSnapshotReader{merged: merged(n)}.Pull(ctx, repo, n)
				}}
				reads := &runReads{reader: reader, pulls: map[string]map[int64]reviewprovider.Pull{}}
				total := int64(17)
				if scenario != "open" {
					total = 32
				}
				width := 4
				if adaptive {
					width = 1
				}
				start := time.Now()
				for n := int64(1); n <= total; {
					head.Store(int32(n))
					numbers := []int64{}
					for k := n; k <= total && len(numbers) < width; k++ {
						numbers = append(numbers, k)
					}
					results := reads.pullWindow(t.Context(), "owner/repo", 801, numbers)
					if len(results) == 0 {
						t.Fatal("empty window")
					}
					for i, result := range results {
						if result.err != nil || !sameTarget(result.pull, 801, numbers[i]) {
							t.Fatal(result.err, result.pull)
						}
						if adaptive {
							width = 1
							if result.pull.Merged && result.pull.State == "closed" {
								width = 4
							}
						}
					}
					n += int64(len(results))
				}
				if scenario == "open" {
					wantCalls, wantCancelled := int32(62), int32(45)
					if adaptive {
						wantCalls, wantCancelled = 17, 0
					}
					if calls.Load() != wantCalls || cancelled.Load() != wantCancelled {
						t.Fatal(calls.Load(), cancelled.Load())
					}
				}
				if scenario == "merged" && (calls.Load() != 32 || cancelled.Load() != 0) {
					t.Fatal(calls.Load(), cancelled.Load())
				}
				t.Logf("metadata=%d cancelled=%d elapsed=%s", calls.Load(), cancelled.Load(), time.Since(start))
			})
		}
	}
}

type adaptiveStackReader struct {
	actualParentSelectionReader
	delay      time.Duration
	boundaries atomic.Int32
}

func (r *adaptiveStackReader) wait(ctx context.Context) error {
	r.boundaries.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.delay):
		return nil
	}
}
func (r *adaptiveStackReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	if err := r.wait(ctx); err != nil {
		return reviewprovider.Repository{}, err
	}
	return r.actualParentSelectionReader.Repository(ctx, id)
}
func (r *adaptiveStackReader) Pull(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
	if err := r.wait(ctx); err != nil {
		return reviewprovider.Pull{}, err
	}
	return r.actualParentSelectionReader.Pull(ctx, repo, n)
}
func (r *adaptiveStackReader) ParentPull(ctx context.Context, repo, branch string) (reviewprovider.Pull, error) {
	// Mirror the GitHub adapter's inventory read then fresh selected-parent GET.
	if err := r.wait(ctx); err != nil {
		return reviewprovider.Pull{}, err
	}
	p, err := r.actualParentSelectionReader.ParentPull(ctx, repo, branch)
	if err != nil {
		return p, err
	}
	return r.Pull(ctx, repo, p.Number)
}

func TestRunSelectionAdaptiveStackSourceBudget(t *testing.T) {
	// Before parent pipelining: 200ms completed 50 boundaries in 10.029s;
	// 400ms exhausted the unchanged 15s budget after 38 boundaries/prefix12.
	// Pipeline retains all 50 boundaries, overlapping only next raw metadata.
	for _, delay := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 450 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			queue := make([]int64, 17)
			pulls := map[int64]reviewprovider.Pull{}
			for i := range queue {
				n := int64(i + 1)
				queue[i] = n
				p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: fmt.Sprintf("%040x", n), HeadRef: fmt.Sprintf("branch-%d", n), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1, IsDraft: new(false)}
				if n > 1 {
					p.BaseRef = fmt.Sprintf("branch-%d", n-1)
					p.BaseSHA = fmt.Sprintf("%040x", n-1)
				}
				pulls[n] = p
			}
			f := selectionFixtureWithQueue(t, queue)
			for n := int64(1); n < 17; n++ {
				frontClose(t, f, n, "hosted", fmt.Sprintf("close-%d", n))
			}
			revision := int64(1)
			_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "adaptive-stack-config", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: queue, Channels: []string{"cli", "hosted"}})
			if err != nil {
				t.Fatal(err)
			}
			receipt, _ := selectionEnqueue(t, f, "adaptive-stack", "hosted", nil)
			r := selectionRead(t, f, receipt.ID)
			c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
			if err != nil {
				t.Fatal(err)
			}
			reader := &adaptiveStackReader{actualParentSelectionReader: actualParentSelectionReader{pulls: pulls}, delay: delay}
			start := time.Now()
			choice, err := f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("17 real parent-linked open PRs delay=%s boundaries=%d prefix=%d state=%s reason=%s source=%s wall=%s", delay, reader.boundaries.Load(), len(choice.prefix), choice.state, choice.reason, choice.sourceElapsed, time.Since(start))
			if delay <= 400*time.Millisecond && (choice.state != "selected" || choice.number != 17 || len(choice.prefix) != 17 || reader.boundaries.Load() != 50 || choice.sourceElapsed >= 15*time.Second) {
				t.Fatal(choice.state, choice.reason, len(choice.prefix), reader.boundaries.Load())
			}
			for _, plan := range choice.prefix {
				if plan.Pull.Number > 1 && (plan.ParentPullNumber != plan.Pull.Number-1 || plan.ParentSHA != fmt.Sprintf("%040x", plan.Pull.Number-1)) {
					t.Fatal("parent identity changed", plan)
				}
			}
			if delay == 450*time.Millisecond && choice.reason != "source_budget_exhausted" {
				t.Fatal(choice.state, choice.reason)
			}
		})
	}
}

func TestRunSelectionAdaptivePauseResetsMergedLookahead(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2, 3, 4, 5})
	schedulingPause(t, f, 2, 0, true)
	receipt, _ := selectionEnqueue(t, f, "adaptive-pause-barrier", "hosted", nil)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		calls.Add(1)
		if n != 1 && n != 3 {
			t.Errorf("speculated across pause: %d", n)
		}
		return selectionSnapshotReader{merged: n == 1}.Pull(ctx, repo, n)
	}}
	choice, err := f.e.chooseRun(t.Context(), r, 801, c, reader)
	if err != nil || choice.state != "selected" || choice.number != 3 || calls.Load() != 2 || len(choice.prefix) != 3 || !choice.prefix[1].schedulingPaused {
		t.Fatal(choice.state, choice.reason, choice.number, calls.Load(), err)
	}
}
