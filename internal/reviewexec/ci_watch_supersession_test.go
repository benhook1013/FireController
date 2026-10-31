package reviewexec

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchFreshSupersessionSurvivesPendingGeneration(t *testing.T) {
	head, base, moved := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, tc := range []struct {
		name, observedHead, observedBase, pinBase, firstBase, state, want, reason string
		version                                                                   uint64
	}{
		{"head moved", moved, base, base, base, "pending", "superseded", "ci_watch_source_changed", 2},
		{"explicit base moved", head, moved, base, base, "pending", "superseded", "ci_watch_source_changed", 2},
		{"implicit base moved", head, moved, "", base, "pending", "superseded", "ci_watch_source_changed", 2},
		{"old cached mismatch", moved, base, base, base, "pending", "pending", "ci_watch_check_pending", 1},
		{"green awaits refresh", head, base, base, base, "passed", "pending", "ci_watch_refresh_pending", 2},
		{"unknown source", "", "", base, base, "pending", "pending", "ci_watch_refresh_pending", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &lifecycleSchedule{}
			target := CIWatchTarget{PullNumber: 42, HeadSHA: head, BaseSHA: tc.pinBase}
			sub := &ciWatchSubscription{scope: core.Scope{ProjectID: "test-project"}, targets: []CIWatchTarget{target}, minimum: []uint64{2}, bases: []string{tc.firstBase}, entries: []*ciWatchEntry{{version: tc.version, wanted: 3, done: 2, result: CIWatchItem{State: tc.state, Reason: "ci_watch_check_pending", ObservedHeadSHA: tc.observedHead, ObservedBaseSHA: tc.observedBase, Checks: []SourceCIContext{}}}}}
			got := s.ciWatchResult(sub)
			if got.State != tc.want || got.Targets[0].State != tc.want || got.Targets[0].Reason != tc.reason || got.Targets[0].CIWatchTarget != target {
				t.Fatal("generation masking lost source identity or admitted stale result", got)
			}
			if tc.version < 2 && got.Targets[0].ObservedHeadSHA != "" {
				t.Fatal("old cache supplied source truth", got)
			}
		})
	}
}

type ciWatchMovedPullReader struct {
	*ciWatchReader
	entered chan struct{}
	release chan struct{}
}

func (r *ciWatchMovedPullReader) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	p, err := r.ciWatchReader.Pull(ctx, repository, number)
	if err != nil {
		return p, err
	}
	close(r.entered)
	select {
	case <-r.release:
		return p, nil
	case <-ctx.Done():
		return reviewprovider.Pull{}, ctx.Err()
	}
}

func TestCIWatchObservedSourceMovementEndsOriginalWaitDespiteNewHint(t *testing.T) {
	f, s, _, r, scope := ciWatchFixture(t)
	before := lifecycleHintCounts(t, f)
	head, base, moved := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, kind := range []string{"head", "base"} {
		t.Run(kind, func(t *testing.T) {
			pull := lifecyclePull(42)
			pull.BaseRef = "stack/parent"
			pull.HeadSHA, pull.BaseSHA = head, base
			if kind == "head" {
				pull.HeadSHA = moved
			} else {
				pull.BaseSHA = moved
			}
			r.mu.Lock()
			r.pulls[42] = pull
			r.mu.Unlock()
			reader := &ciWatchMovedPullReader{ciWatchReader: r, entered: make(chan struct{}), release: make(chan struct{})}
			s.readers["test-capability"] = reader
			target := CIWatchTarget{PullNumber: 42, HeadSHA: head, BaseSHA: base}
			out, cancel := startCIWatch(t, f, scope, []CIWatchTarget{target})
			defer cancel()
			ciWatchRegistered(t, s, 1)
			claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
			if err != nil || claim == nil || claim.watch == nil {
				t.Fatal("no controlled watch claim", claim, err)
			}
			observed := make(chan error, 1)
			go func() { observed <- f.e.observeLifecycle(t.Context(), *claim, s) }()
			select {
			case <-reader.entered:
			case <-time.After(time.Second):
				t.Fatal("source read did not start")
			}
			// This hint only requests newer collection; it cannot erase a freshly
			// observed mismatch against this original subscription's immutable pins.
			hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, HeadSHA: pull.HeadSHA, Metadata: true, CI: true})
			close(reader.release)
			if err := <-observed; err != nil {
				t.Fatal(err)
			}
			reply := receiveCIWatch(t, out)
			if reply.err != nil || reply.value.State != "superseded" || reply.value.TimedOut || reply.value.Targets[0].Reason != "ci_watch_source_changed" || reply.value.Targets[0].CIWatchTarget != target || reply.value.Targets[0].ObservedHeadSHA != pull.HeadSHA || reply.value.Targets[0].ObservedBaseSHA != pull.BaseSHA {
				t.Fatal("fresh source supersession remained pending", reply)
			}
			ciWatchRegistered(t, s, 0)
		})
	}
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls != 0 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("source mismatch collected CI or mutated review authority", calls)
	}
}
