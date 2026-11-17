package reviewexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type parentPipelineReader struct {
	pull   func(context.Context, string, int64) (reviewprovider.Pull, error)
	parent func(context.Context, string, string) (reviewprovider.Pull, error)
}

func (r *parentPipelineReader) Repository(_ context.Context, n int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{ID: n, FullName: "owner/repo", DefaultBranch: "main"}, nil
}
func (r *parentPipelineReader) Pull(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
	return r.pull(ctx, repo, n)
}
func (r *parentPipelineReader) ParentPull(ctx context.Context, repo, branch string) (reviewprovider.Pull, error) {
	return r.parent(ctx, repo, branch)
}
func pipelinePull(n int64) reviewprovider.Pull {
	return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: fmt.Sprintf("%040x", n), BaseSHA: fmt.Sprintf("%040x", n-1), HeadRef: fmt.Sprintf("branch-%d", n), BaseRef: fmt.Sprintf("branch-%d", n-1), ChangedFiles: 1, IsDraft: new(false)}
}
func pipelineReads(reader TargetReader) *runReads {
	p := pipelinePull(2)
	return &runReads{reader: reader, pulls: map[string]map[int64]reviewprovider.Pull{"owner/repo": {2: p}}, parentHint: &runParentHint{repository: "owner/repo", repositoryID: 801, current: 2, next: 3, head: p.HeadSHA, base: p.BaseSHA, branch: p.BaseRef}}
}
func TestRunSelectionParentPipelineJoinsAndValidates(t *testing.T) {
	for _, mode := range []string{"success", "foreign", "wrong-number", "error", "slow-suffix", "parent-error", "cancelled", "expired-success", "late-success", "parent-is-next"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			var active atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &parentPipelineReader{
				pull: func(ctx context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					active.Add(1)
					defer active.Add(-1)
					close(entered)
					p := pipelinePull(n)
					switch mode {
					case "foreign":
						p.RepositoryID++
					case "wrong-number":
						p.Number++
					case "error":
						return reviewprovider.Pull{}, errors.New("unused source error")
					case "late-success":
						<-ctx.Done()
						return p, nil
					case "slow-suffix", "cancelled":
						<-ctx.Done()
						return reviewprovider.Pull{}, ctx.Err()
					}
					return p, nil
				},
				parent: func(ctx context.Context, _ string, _ string) (reviewprovider.Pull, error) {
					select {
					case <-entered:
					case <-ctx.Done():
						return reviewprovider.Pull{}, ctx.Err()
					}
					if mode == "slow-suffix" {
						time.Sleep(20 * time.Millisecond)
					}
					if mode == "expired-success" {
						cancel()
					}
					if mode == "cancelled" {
						cancel()
						return reviewprovider.Pull{}, ctx.Err()
					}
					if mode == "parent-error" {
						return reviewprovider.Pull{}, errors.New("required source error")
					}
					p := pipelinePull(1)
					if mode == "parent-is-next" {
						p = pipelinePull(3)
						p.HeadSHA = strings.Repeat("f", 40)
					}
					return p, nil
				},
			}
			reads := pipelineReads(reader)
			start := time.Now()
			p, err := reads.ParentPull(ctx, "owner/repo", "branch-1")
			if active.Load() != 0 || reads.parentHint != nil || time.Since(start) > time.Second {
				t.Fatal("reader leaked", active.Load(), time.Since(start))
			}
			if mode == "parent-error" {
				if err == nil || err.Error() != "required source error" {
					t.Fatal(err)
				}
			} else if mode == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			next, found := reads.pulls["owner/repo"][3]
			if mode == "success" && (!found || next.Number != 3) {
				t.Fatal("successful exact metadata not retained")
			}
			if mode == "parent-is-next" {
				if !found || next.HeadSHA != p.HeadSHA {
					t.Fatal("prefetch overwrote required fresh parent")
				}
			} else if mode != "success" && found {
				t.Fatal("unusable speculative identity retained", next)
			}
		})
	}
}
func TestRunSelectionParentPipelineHintGuards(t *testing.T) {
	for _, mode := range []string{"parent-cache", "next-cache", "wrong-branch", "wrong-repository", "changed-current", "draft-current"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			reader := &parentPipelineReader{
				pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
					calls.Add(1)
					return pipelinePull(3), nil
				},
				parent: func(context.Context, string, string) (reviewprovider.Pull, error) { return pipelinePull(1), nil },
			}
			reads := pipelineReads(reader)
			switch mode {
			case "parent-cache":
				reads.parents = map[string]reviewprovider.Pull{"owner/repo\x00branch-1": pipelinePull(1)}
			case "next-cache":
				reads.pulls["owner/repo"][3] = pipelinePull(3)
			case "wrong-branch":
				reads.parentHint.branch = "other"
			case "wrong-repository":
				reads.parentHint.repository = "other/repo"
			case "changed-current":
				p := reads.pulls["owner/repo"][2]
				p.HeadSHA = strings.Repeat("e", 40)
				reads.pulls["owner/repo"][2] = p
			case "draft-current":
				p := reads.pulls["owner/repo"][2]
				p.IsDraft = new(true)
				reads.pulls["owner/repo"][2] = p
			}
			if _, err := reads.ParentPull(t.Context(), "owner/repo", "branch-1"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 || reads.parentHint != nil {
				t.Fatal("guard launched or retained hint", calls.Load())
			}
		})
	}
}

func TestRunSelectionParentPipelineReopenedRoundsRecheck(t *testing.T) {
	f := selectionFixtureWithQueue(t, []int64{1, 2, 3})
	frontClose(t, f, 1, "hosted", "close-root")
	frontClose(t, f, 2, "hosted", "close-child")
	revision := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "pipeline-config", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{1, 2, 3}, Channels: []string{"hosted", "cli"}}); err != nil {
		t.Fatal(err)
	}
	receipt, _ := selectionEnqueue(t, f, "pipeline-reopen", "hosted", nil)
	r := selectionRead(t, f, receipt.ID)
	c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	reader := &parentPipelineReader{
		pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
			p := pipelinePull(n)
			if n == 1 {
				p.BaseRef = "main"
			}
			if n == 3 {
				close(entered)
			}
			return p, nil
		},
		parent: func(ctx context.Context, _ string, branch string) (reviewprovider.Pull, error) {
			if branch != "branch-1" {
				t.Errorf("unexpected parent lookup %s", branch)
			}
			select {
			case <-entered:
			case <-ctx.Done():
				return reviewprovider.Pull{}, ctx.Err()
			}
			old := roundRead(t, f, 2, "hosted")
			minimum, maximum := int64(1), int64(3)
			_, err := f.e.ConfigureRoundControl(ctx, f.scope(t, attemptProject, f.admin, true), "reopen-during-parent", 2, "hosted", RoundControlRequest{ExpectedRevision: &old.Revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
			p := pipelinePull(1)
			p.BaseRef = "main"
			return p, err
		},
	}
	choice, err := f.e.chooseRunWithSourceBudget(t.Context(), r, 801, c, reader, time.Second)
	if err != nil || choice.state != "selected" || choice.number != 2 || len(choice.prefix) != 2 {
		t.Fatal(choice.state, choice.reason, choice.number, len(choice.prefix), err)
	}
	if choice.prefix[1].ParentPullNumber != 1 || choice.prefix[1].ParentSHA != pipelinePull(1).HeadSHA {
		t.Fatal("parent comparison changed")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("prefetch dispatched work")
	}
}

func TestRunSelectionParentPipelineSelectionBarriers(t *testing.T) {
	for _, mode := range []string{"paused-next", "expected-bound", "retained-next"} {
		t.Run(mode, func(t *testing.T) {
			var f *attemptFixture
			if mode == "retained-next" {
				// Only PR3 has a verified retained merged identity.
				f = selectionHistoryFixture(t, []int64{1, 2, 3}, 0)
				p1, p2, p3 := projectionPull(1), projectionPull(2), projectionPull(3)
				p3.State = "merged"
				req := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(1), RepositoryID: 801, Source: "pipeline-history", ObservedAt: "2026-01-02T00:00:00Z", Queue: []reviews.PullInput{p1, p2, p3}}
				response := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "pipeline-history", encoded(req))
				if response.Code != 201 {
					t.Fatal(response.Code, response.Body.String())
				}
			} else {
				f = selectionFixtureWithQueue(t, []int64{1, 2, 3})
			}
			for n := int64(1); n <= 2; n++ {
				rounds := roundRead(t, f, n, "hosted")
				_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("barrier-close-%d", n), n, "hosted", RoundControlRequest{ExpectedRevision: &rounds.Revision, Action: "close"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "paused-next" {
				schedulingPause(t, f, 3, 0, true)
			}
			revision := int64(1)
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "barrier-config", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{1, 2, 3}, Channels: []string{"hosted", "cli"}}); err != nil {
				t.Fatal(err)
			}
			var expected *int64
			if mode == "expected-bound" {
				n := int64(2)
				expected = &n
			}
			receipt, _ := selectionEnqueue(t, f, "pipeline-barrier", "hosted", expected)
			r := selectionRead(t, f, receipt.ID)
			c, err := f.e.configurationRevision(t.Context(), r.ProjectID, r.ConfigurationRevision)
			if err != nil {
				t.Fatal(err)
			}
			var nextCalls, parentCalls atomic.Int32
			reader := &parentPipelineReader{
				pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					if n == 3 {
						nextCalls.Add(1)
					}
					p := pipelinePull(n)
					if n == 1 {
						p.BaseRef = "main"
					}
					return p, nil
				},
				parent: func(context.Context, string, string) (reviewprovider.Pull, error) {
					parentCalls.Add(1)
					p := pipelinePull(1)
					p.BaseRef = "main"
					return p, nil
				},
			}
			choice, err := f.e.chooseRun(t.Context(), r, 801, c, reader)
			if err != nil || nextCalls.Load() != 0 || parentCalls.Load() != 1 || choice.state == "selected" {
				t.Fatal(mode, choice.state, choice.reason, nextCalls.Load(), parentCalls.Load(), err)
			}
		})
	}
}
