//go:build cohortintegration

package reviewexec

import (
	"context"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLifecycleSuiteQualificationUsesActualCredentialAndReservation(t *testing.T) {
	for _, tc := range []struct{ enabled, watching bool }{{false, false}, {false, true}} {
		enabled, watching := tc.enabled, tc.watching
		t.Run(fmt.Sprintf("batch%t_watch%t", enabled, watching), func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			g, targets, calls := engineRequiredNewDistinctCIFixture(t, "", func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				if !s.lanes[lifecycleEnrichment].projects[attemptProject] || len(s.lanes[lifecycleEnrichment].accounts) != 1 {
					t.Error("account/project reservation missing during provider I/O")
				}
			})
			var sourceCalls atomic.Int64
			r := &requiredEngineReader{github: g, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				sourceCalls.Add(1)
				p := lifecyclePull(n)
				if n == 4 {
					p.HeadSHA = strings.Repeat("d", 40)
				}
				return p, nil
			}}}}
			r.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return sourceCITestObservation(target, "failed"), nil
			}
			lifecycleTick(t, f, s, r)
			lifecycleTick(t, f, s, r)
			for _, target := range targets {
				var before reviews.PullResponse
				projectionRead(t, f, fmt.Sprintf("/pulls/%d", target.PullNumber), &before)
				if before.Pull.CIState == "success" {
					t.Fatal("seed must not already be successful")
				}
			}
			r.gateReady = true
			clock.Add(6 * time.Minute)
			s.options.BatchCIWitnesses = enabled
			s.options.QualifyCIBatching = true
			calls.Store(0)
			sourceCalls.Store(0)
			first, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleEnrichment)
			if err != nil || first == nil {
				t.Fatal(first, err)
			}
			var watched <-chan ciWatchReply
			if watching {
				s.readers = map[string]TargetReader{"test-capability": r}
				attachLifecycleHints(t, f, s)
				var cancelWatch context.CancelFunc
				watched, cancelWatch = startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: first.number, HeadSHA: first.comparison.head, BaseSHA: first.comparison.baseSHA}})
				defer cancelWatch()

				ciWatchRegistered(t, s, 1)
			}
			if err = f.e.observeLifecycleCohort(t.Context(), *first, map[string]TargetReader{"test-capability": r}, s); err != nil {
				t.Fatal(err)
			}
			want := int64(53)
			if watching {
				want = 48
			}
			if enabled && !watching {
				want = 40
			}
			if watching {
				reply := receiveCIWatch(t, watched)
				if reply.err != nil || reply.value.State != "passed" {
					t.Fatal("existing watch lost fresh sharing", reply)
				}
			}
			if calls.Load() != want || sourceCalls.Load() != 2 {
				t.Fatal("collector/preparation requests", calls.Load(), sourceCalls.Load(), want)
			}
			for _, target := range targets {
				var persisted reviews.PullResponse
				projectionRead(t, f, fmt.Sprintf("/pulls/%d", target.PullNumber), &persisted)
				if persisted.Pull.CIState != "success" {
					t.Fatal("persisted required CI did not publish", target, persisted.Pull.CIState)
				}
				d := s.due[lifecycleKey{attemptProject, target.PullNumber}]
				if d.ciGateState != "success" || d.ciError != "" || d.ciHead != target.HeadSHA || !d.ci.After(clock.Now()) {
					t.Fatal("required CI not published", target, d.ciGateState, d.ciError, d.ciHead)
				}
			}
			counts := s.batching.counts
			if !enabled {
				if counts != [batchOutcomeCount]uint64{} {
					t.Fatal("disabled batch diagnostics recorded", counts)
				}
			} else if watching {
				if counts[batchEligible] != 1 || counts[batchWatchFallback] != 1 || counts[batchPrepared] != 0 || counts[batchPublishedMember] != 0 {
					t.Fatal("watch fallback diagnostics", counts)
				}
			} else if counts[batchEligible] != 1 || counts[batchPrepared] != 1 || counts[batchPublishedMember] != 2 || counts[batchDiscardedMember] != 0 {
				t.Fatal("prepared member diagnostics", counts)
			}
			if !watching {
				if len(s.qualificationClaimed) != 1 {
					t.Fatal("qualification not retained once")
				}
				clock.Add(6 * time.Minute)
				calls.Store(0)
				again, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleEnrichment)
				if err != nil || again == nil {
					t.Fatal(again, err)
				}
				if err = f.e.observeLifecycleCohort(t.Context(), *again, map[string]TargetReader{"test-capability": r}, s); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 48 {
					t.Fatal("one-shot qualification repeated", calls.Load())
				}
			} else if len(s.qualificationClaimed) != 0 {
				t.Fatal("watched cohort qualified")
			}
			if len(s.lanes[lifecycleEnrichment].accounts) != 0 || len(s.lanes[lifecycleEnrichment].projects) != 0 {
				t.Fatal("reservation leak")
			}
		})
	}
}

func TestLifecycleSuiteQualificationLimitPreservesCooldownAndEvidence(t *testing.T) {
	for _, tc := range []struct{ enabled, watching bool }{{false, false}} {
		enabled, watching := tc.enabled, tc.watching
		t.Run(fmt.Sprintf("batch%t_watch%t", enabled, watching), func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			g, targets, calls := engineRequiredNewDistinctCIFixture(t, "qualification_limit", func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				if !s.lanes[lifecycleEnrichment].projects[attemptProject] || len(s.lanes[lifecycleEnrichment].accounts) != 1 {
					t.Error("account/project reservation missing during provider I/O")
				}
			})
			var sourceCalls atomic.Int64
			r := &requiredEngineReader{github: g, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				sourceCalls.Add(1)
				p := lifecyclePull(n)
				if n == 4 {
					p.HeadSHA = strings.Repeat("d", 40)
				}
				return p, nil
			}}}}
			r.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return sourceCITestObservation(target, "failed"), nil
			}
			lifecycleTick(t, f, s, r)
			lifecycleTick(t, f, s, r)
			for _, target := range targets {
				var before reviews.PullResponse
				projectionRead(t, f, fmt.Sprintf("/pulls/%d", target.PullNumber), &before)
				if before.Pull.CIState == "success" {
					t.Fatal("seed must not already be successful")
				}
			}
			r.gateReady = true
			clock.Add(6 * time.Minute)
			s.options.BatchCIWitnesses = enabled
			s.options.QualifyCIBatching = true
			calls.Store(0)
			sourceCalls.Store(0)
			first, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleEnrichment)
			if err != nil || first == nil {
				t.Fatal(first, err)
			}
			var watched <-chan ciWatchReply
			if watching {
				s.readers = map[string]TargetReader{"test-capability": r}
				attachLifecycleHints(t, f, s)
				var cancelWatch context.CancelFunc
				watched, cancelWatch = startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: first.number, HeadSHA: first.comparison.head, BaseSHA: first.comparison.baseSHA}})
				defer cancelWatch()

				ciWatchRegistered(t, s, 1)
			}
			beforeDue := map[lifecycleKey]lifecycleDue{}
			for key, row := range s.due {
				beforeDue[key] = row
			}
			if err = f.e.observeLifecycleCohort(t.Context(), *first, map[string]TargetReader{"test-capability": r}, s); err != nil {
				t.Fatal(err)
			}
			want := int64(3)
			if watching {
				want = 48
			}
			if enabled && !watching {
				want = 40
			}
			if watching {
				reply := receiveCIWatch(t, watched)
				if reply.err != nil || reply.value.State != "passed" {
					t.Fatal("existing watch lost fresh sharing", reply)
				}
			}
			if calls.Load() != want || sourceCalls.Load() != 2 {
				t.Fatal("collector/preparation requests", calls.Load(), sourceCalls.Load(), want)
			}
			for _, target := range targets {
				var persisted reviews.PullResponse
				projectionRead(t, f, fmt.Sprintf("/pulls/%d", target.PullNumber), &persisted)
				if persisted.Pull.CIState != "failure" {
					t.Fatal("persisted required CI did not publish", target, persisted.Pull.CIState)
				}
				d := s.due[lifecycleKey{attemptProject, target.PullNumber}]
				if d != beforeDue[lifecycleKey{attemptProject, target.PullNumber}] {
					t.Fatal("required CI not published", target, d.ciGateState, d.ciError, d.ciHead)
				}
			}
			counts := s.batching.counts
			if !enabled {
				if counts != [batchOutcomeCount]uint64{} {
					t.Fatal("disabled batch diagnostics recorded", counts)
				}
			} else if watching {
				if counts[batchEligible] != 1 || counts[batchWatchFallback] != 1 || counts[batchPrepared] != 0 || counts[batchPublishedMember] != 0 {
					t.Fatal("watch fallback diagnostics", counts)
				}
			} else if counts[batchEligible] != 1 || counts[batchPrepared] != 1 || counts[batchPublishedMember] != 2 || counts[batchDiscardedMember] != 0 {
				t.Fatal("prepared member diagnostics", counts)
			}
			if !watching {
				if !clock.Now().Before(s.accounts[first.account].until) {
					t.Fatal("qualification limit lost shared cooldown")
				}
				if len(s.qualificationClaimed) != 1 {
					t.Fatal("qualification not retained once")
				}
				clock.Add(6 * time.Minute)
				calls.Store(0)
				again, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleEnrichment)
				if err != nil || again == nil {
					t.Fatal(again, err)
				}
				if err = f.e.observeLifecycleCohort(t.Context(), *again, map[string]TargetReader{"test-capability": r}, s); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 48 {
					t.Fatal("one-shot qualification repeated", calls.Load())
				}
			} else if len(s.qualificationClaimed) != 0 {
				t.Fatal("watched cohort qualified")
			}
			if len(s.lanes[lifecycleEnrichment].accounts) != 0 || len(s.lanes[lifecycleEnrichment].projects) != 0 {
				t.Fatal("reservation leak")
			}
		})
	}
}
