package reviewexec

import (
	"fmt"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
)

func TestLifecyclePruningPreservesInFlightHintGeneration(t *testing.T) {
	for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
		t.Run(fmt.Sprint(lane), func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			attachLifecycleHints(t, f, s)
			reader := &sourceCITestReader{}
			readers := map[string]TargetReader{"test-capability": reader}
			key := lifecycleKey{attemptProject, 2}
			hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, HeadSHA: lifecyclePull(2).HeadSHA, Metadata: true, CI: true})
			target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
			if err != nil || target == nil || target.hintGeneration != 1 {
				t.Fatal("hint claim not established", target, err)
			}
			configure := func(revision int64, queue []int64) {
				t.Helper()
				_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("prune-%d", revision), Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: queue, Channels: []string{"cli", "hosted"}, Paused: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			configure(2, []int64{4})
			other := lifecycleEnrichment
			if lane == other {
				other = lifecycleMetadata
			}
			peer, err := f.e.claimLifecycleLane(t.Context(), readers, s, other)
			if err != nil || peer == nil || peer.number != 4 {
				t.Fatal("independent PR not claimed", peer, err)
			}
			s.finish(*peer, "open", peer.comparison.head, false, false, nil)
			s.finish(*target, "open", target.comparison.head, false, false, nil)
			due := s.due[key]
			if due.hints[lane].wanted != 1 || due.hints[lane].done != 1 {
				t.Fatal("pruning lost in-flight generation", due.hints[lane])
			}
			configure(3, []int64{2})
			clock.Add(5 * time.Second)
			if got, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane); err != nil || got != nil {
				t.Fatal("reenrollment created phantom five-second hint", got, err)
			}
			if len(s.lanes[lane].projects) != 0 || len(s.lanes[lane].accounts) != 0 {
				t.Fatal("completion retained ownership")
			}
			hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, HeadSHA: lifecyclePull(2).HeadSHA, Metadata: true, CI: true})
			next, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
			if err != nil || next == nil || next.hintGeneration != 2 {
				t.Fatal("new hint lost after reenrollment", next, err)
			}
			hintLifecycle(t, f, githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{2}, HeadSHA: lifecyclePull(2).HeadSHA, Metadata: true, CI: true})
			s.finish(*next, "open", next.comparison.head, false, false, nil)
			if hint := s.due[key].hints[lane]; hint.wanted != 3 || hint.done != 2 {
				t.Fatal("in-flight hint was consumed", hint)
			}
			clock.Add(5 * time.Second)
			last, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
			if err != nil || last == nil || last.hintGeneration != 3 {
				t.Fatal("later generation not claimable", last, err)
			}
			s.finish(*last, "open", last.comparison.head, false, false, nil)
			// Once no claim owns this PR, later removal can reclaim its schedule entry.
			configure(4, []int64{4})
			if got, err := f.e.claimLifecycleLane(t.Context(), readers, s, other); err != nil {
				t.Fatal(err)
			} else if got != nil {
				if got.number != 4 {
					t.Fatal("removed PR reclaimed", got)
				}
				s.finish(*got, "open", got.comparison.head, false, false, nil)
			}
			if _, exists := s.due[key]; exists {
				t.Fatal("completed removed claim was never pruned")
			}
		})
	}
}
