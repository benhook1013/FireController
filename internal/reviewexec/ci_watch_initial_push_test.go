package reviewexec

import (
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
)

func TestCIWatchInitialBranchPushSurvivesInflightObservation(t *testing.T) {
	f, s, clock, reader, scope := ciWatchFixture(t)
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.unregisterCIWatch(sub)
	reader.states[42] = "satisfied"
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	reader.block, reader.entered = release, entered
	claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || claim == nil || claim.watch == nil {
		t.Fatal("initial watch not claimed", claim, err)
	}
	// Completion is held after the pull was read but before its base ref is
	// published. The push names only a branch/new base commit, not the PR head.
	done := make(chan error, 1)
	go func() { done <- f.e.observeLifecycle(t.Context(), *claim, s) }()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial observation did not reach provider")
	}
	entry := sub.entries[0]
	s.mu.Lock()
	wanted := entry.wanted
	s.mu.Unlock()
	s.hintCIWatches(githubhook.Hint{RepositoryID: 999, BranchRef: "stack/parent", CI: true})
	s.mu.Lock()
	foreignChanged := entry.wanted != wanted
	s.mu.Unlock()
	if foreignChanged {
		t.Fatal("foreign repository push woke watch")
	}
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, BranchRef: "stack/parent", HeadSHA: strings.Repeat("c", 40), CI: true})
	pull := lifecyclePull(42)
	pull.BaseRef, pull.BaseSHA = "stack/parent", strings.Repeat("c", 40)
	reader.mu.Lock()
	reader.pulls[42] = pull
	reader.block, reader.entered = nil, nil
	reader.mu.Unlock()
	close(release)
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial observation did not finish")
	}
	if entry.wanted == entry.done || entry.wanted != wanted+1 {
		t.Fatal("initial completion consumed or missed later push", entry.wanted, entry.done)
	}
	// Event coalescing still applies; do not wait for the minute fallback.
	// The refreshed pull proves supersession before another CI provider read.
	clock.Add(5 * time.Second)
	watchTick(t, f, s)
	got := s.ciWatchResult(sub)
	if got.State != "superseded" || got.Targets[0].ObservedBaseSHA != pull.BaseSHA || reader.calls != 1 {
		t.Fatal("branch push did not promptly refresh the pinned comparison", got, reader.calls)
	}
	wanted = entry.wanted
	s.hintCIWatches(githubhook.Hint{RepositoryID: 801, BranchRef: "unrelated", HeadSHA: strings.Repeat("d", 40), CI: true})
	if entry.wanted != wanted {
		t.Fatal("known base ref did not restore narrow branch filtering")
	}
}
