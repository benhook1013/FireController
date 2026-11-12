package reviewexec

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchUnpublishedMergeReader struct {
	*ciWatchReader
	phase        string
	entered      chan struct{}
	release      chan struct{}
	repositories int
}

func (r *ciWatchUnpublishedMergeReader) pause(ctx context.Context) error {
	close(r.entered)
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *ciWatchUnpublishedMergeReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.repositories++
	if r.phase == "final" && r.repositories == 2 {
		if err := r.pause(ctx); err != nil {
			return reviewprovider.Repository{}, err
		}
	}
	return r.ciWatchReader.Repository(ctx, id)
}
func (r *ciWatchUnpublishedMergeReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	o, err := r.ciWatchReader.ObserveCIGate(ctx, target, gate)
	if err != nil {
		return o, err
	}
	if r.phase == "observe" && r.calls == 1 {
		if err := r.pause(ctx); err != nil {
			return reviewprovider.CIObservation{}, err
		}
	}
	merge := strings.Repeat("c", 40)
	o.Selection, o.SelectedCommitSHA, o.MergeState = "test_merge", merge, "verified"
	o.Candidate.MergeState, o.Candidate.MergeSHA, o.Candidate.MergeBaseParentSHA = "verified", merge, target.BaseSHA
	o.Results.HeadSHA, o.Actions.HeadSHA = merge, merge
	o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
	o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	return o, nil
}

func TestCIWatchUnpublishedMergeHintReconcilesBeforePublication(t *testing.T) {
	for _, phase := range []string{"observe", "final"} {
		for _, kind := range []string{"matching", "unrelated", "foreign", "explicit other PR", "overflow", "duplicate", "canceled"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				f, s, clock, base, scope := ciWatchFixture(t)
				base.states[42] = "satisfied"
				r := &ciWatchUnpublishedMergeReader{ciWatchReader: base, phase: phase, entered: make(chan struct{}), release: make(chan struct{})}
				s.readers["test-capability"] = r
				binding, err := f.e.ciWatchBinding(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}})
				if err != nil {
					t.Fatal(err)
				}
				defer s.unregisterCIWatch(sub)
				claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
				if err != nil || claim == nil || claim.watch == nil {
					t.Fatal(claim, err)
				}
				done := make(chan error, 1)
				go func() { done <- f.e.observeLifecycle(t.Context(), *claim, s) }()
				defer func() {
					select {
					case <-r.release:
					default:
						close(r.release)
					}
				}()
				select {
				case <-r.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider barrier not reached")
				}
				hint := githubhook.Hint{RepositoryID: 801, HeadSHA: strings.Repeat("c", 40), CI: true}
				switch kind {
				case "unrelated", "overflow":
					hint.HeadSHA = strings.Repeat("d", 40)
				case "foreign":
					hint.RepositoryID = 999
				case "explicit other PR":
					hint.PullNumbers = []int64{99}
				}
				s.hintCIWatches(hint)
				if kind == "overflow" {
					for i := 0; i < 40; i++ {
						hint.HeadSHA = fmt.Sprintf("%040x", i+1)
						s.hintCIWatches(hint)
					}
				}
				if kind == "overflow" {
					hint.HeadSHA = strings.Repeat("c", 40)
					s.hintCIWatches(hint)
				}
				if kind == "duplicate" {
					for i := 0; i < 40; i++ {
						s.hintCIWatches(hint)
					}
				}
				s.mu.Lock()
				retained, overflow := len(sub.entries[0].inflightSHAHints), sub.entries[0].inflightSHAOverflow
				if kind == "canceled" {
					sub.entries[0].cancel()
				}
				s.mu.Unlock()
				if retained > 32 || kind == "overflow" && !overflow || kind == "duplicate" && (retained != 1 || overflow) {
					t.Fatal("hint retention is not bounded/deduplicated", retained, overflow)
				}
				close(r.release)
				select {
				case err = <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("observation did not finish")
				}
				got := s.ciWatchResult(sub)
				wantRefresh := kind == "matching" || kind == "overflow" || kind == "duplicate"
				if kind == "canceled" {
					if got.State != "pending" || sub.entries[0].version != 0 {
						t.Fatal("canceled read published proof", got)
					}
				} else if wantRefresh {
					if got.State != "pending" || got.Targets[0].Reason != "ci_watch_refresh_pending" {
						t.Fatal("stale passing merge escaped hint", got)
					}
					next, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
					if err != nil || next != nil {
						t.Fatal("hint bypassed event coalescing", next, err)
					}
					clock.Add(5 * time.Second)
					watchTick(t, f, s)
					if got = s.ciWatchResult(sub); got.State != "passed" || base.calls != 2 {
						t.Fatal("retained hint failed to refresh", got, base.calls)
					}
				} else if got.State != "passed" || base.calls != 1 {
					t.Fatal("unrelated hint invalidated proof", got, base.calls)
				}
				if len(sub.entries[0].inflightSHAHints) != 0 || sub.entries[0].inflightSHAOverflow {
					t.Fatal("completed collection leaked hints")
				}
			})
		}
	}
}

func TestCIWatchRearmCarriesObservedImplicitBase(t *testing.T) {
	f, s, clock, reader, scope := ciWatchFixture(t)
	reader.states[42] = "satisfied"
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	target := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}
	sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	watchTick(t, f, s)
	first := s.ciWatchResult(sub)
	if first.State != "passed" || first.Targets[0].BaseSHA != "" || first.Targets[0].ObservedBaseSHA != strings.Repeat("b", 40) {
		t.Fatal("implicit base contract changed", first)
	}
	s.unregisterCIWatch(sub)
	target.BaseSHA = first.Targets[0].ObservedBaseSHA
	target.AfterObservationKey = first.Targets[0].ObservationKey
	pull := lifecyclePull(42)
	pull.BaseSHA = strings.Repeat("d", 40)
	reader.pulls[42] = pull
	rearmed, err := s.registerCIWatch(scope, binding, []CIWatchTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	defer s.unregisterCIWatch(rearmed)
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	if got := s.ciWatchResult(rearmed); got.State != "superseded" {
		t.Fatal("carried implicit base silently repinned", got)
	}
}

func TestCIWatchUnpublishedMergeHintRetainsInflightDemand(t *testing.T) {
	f, s, _, _, retained, register := ciWatchDemandFixture(t)
	gone := register()
	claim, err := f.e.claimCIWatch(t.Context(), s.readers, s)
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	defer claim.watch.entry.cancel()
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, HeadSHA: strings.Repeat("c", 40), CI: true})
	s.unregisterCIWatch(gone)
	if claim.watch.ctx.Err() != nil || !ciWatchHasDemand(retained.entries[0], s.options.Now()) {
		t.Fatal("subscriber departure discarded an unreconciled commit hint")
	}
}
