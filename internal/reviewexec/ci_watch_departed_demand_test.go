package reviewexec

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
)

func ciWatchDemandFixture(t *testing.T) (*attemptFixture, *lifecycleSchedule, *lifecycleTestClock, *ciWatchReader, *ciWatchSubscription, func() *ciWatchSubscription) {
	t.Helper()
	f, s, clock, reader, scope := ciWatchFixture(t)
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	register := func() *ciWatchSubscription {
		t.Helper()
		sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
		if err != nil {
			t.Fatal(err)
		}
		return sub
	}
	retained := register()
	t.Cleanup(func() { s.unregisterCIWatch(retained) })
	reader.states[42] = "satisfied"
	watchTick(t, f, s)
	if s.ciWatchResult(retained).State != "passed" {
		t.Fatal("fixture has no fresh passing observation")
	}
	clock.Add(5 * time.Second)
	return f, s, clock, reader, retained, register
}

func TestCIWatchUnregisterCancelsOnlyUndemandedCollection(t *testing.T) {
	for _, demand := range []string{"none", "subscriber", "ci_hint", "inflight_ci_hint", "fallback"} {
		t.Run(demand, func(t *testing.T) {
			f, s, clock, reader, retained, register := ciWatchDemandFixture(t)
			original := s.ciWatchResult(retained)
			gone := register()
			if demand == "subscriber" {
				other := register()
				defer s.unregisterCIWatch(other)
			}
			hint := githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, CI: true}
			if demand == "ci_hint" {
				s.hintCIWatches(hint)
			}
			if demand == "fallback" {
				clock.Add(ciWatchFallbackInterval)
			}
			reader.block, reader.entered = make(chan struct{}), make(chan struct{}, 1)
			claim, err := f.e.claimCIWatch(t.Context(), s.readers, s)
			if err != nil || claim == nil {
				t.Fatal("fresh subscriber did not acquire a collection", claim, err)
			}
			defer claim.watch.entry.cancel()
			done := make(chan error, 1)
			go func() { done <- f.e.observeCIWatch(t.Context(), *claim, s) }()
			select {
			case <-reader.entered:
			case <-time.After(time.Second):
				t.Fatal("collection did not reach provider")
			}
			if demand == "inflight_ci_hint" {
				s.hintCIWatches(hint)
			}
			s.unregisterCIWatch(gone)
			if canceled := claim.watch.ctx.Err() != nil; canceled != (demand == "none") {
				close(reader.block)
				t.Fatalf("collection cancellation %v does not match remaining demand %q", canceled, demand)
			}
			var replacement *ciWatchSubscription
			if demand == "none" {
				// An owner arriving after cancellation still needs its own collection.
				replacement = register()
				defer s.unregisterCIWatch(replacement)
			}
			close(reader.block)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled or released collection did not finish")
			}
			entry := retained.entries[0]
			if entry.inflight || s.lanes[lifecycleEnrichment].accounts[entry.key.binding.account] || s.lanes[lifecycleEnrichment].projects[entry.key.binding.project] {
				t.Fatal("collection retained its lane reservation")
			}
			if demand == "none" {
				if got := s.ciWatchResult(retained); !reflect.DeepEqual(got, original) {
					t.Fatal("canceled collection overwrote completed observation", got, original)
				}
				if got := s.ciWatchResult(replacement); got.State != "pending" || got.Targets[0].ObservedAt != "" {
					t.Fatal("canceled collection supplied freshness to a new subscriber", got)
				}
				clock.Add(5 * time.Second)
				watchTick(t, f, s)
				if s.ciWatchResult(replacement).State != "passed" {
					t.Fatal("new demand was lost during canceled teardown")
				}
			} else if demand == "inflight_ci_hint" {
				if entry.wanted == entry.done {
					t.Fatal("completion consumed a later CI hint")
				}
			} else if s.ciWatchResult(retained).State != "passed" {
				t.Fatal("remaining demand lost its provider result")
			}
		})
	}
}

func TestCIWatchClaimRechecksDemandAfterAuthorization(t *testing.T) {
	for _, demand := range []string{"none", "subscriber", "ci_hint", "fallback"} {
		t.Run(demand, func(t *testing.T) {
			f, s, clock, _, retained, register := ciWatchDemandFixture(t)
			gone := register()
			release := holdCIWatchReadPool(t, f)
			initial := f.store.DB().Stats().WaitCount
			type reply struct {
				target *lifecycleTarget
				err    error
			}
			done := make(chan reply, 1)
			go func() { target, err := f.e.claimCIWatch(t.Context(), s.readers, s); done <- reply{target, err} }()
			deadline := time.Now().Add(time.Second)
			for f.store.DB().Stats().WaitCount == initial && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if f.store.DB().Stats().WaitCount == initial {
				t.Fatal("claim did not reach blocked authorization")
			}
			s.unregisterCIWatch(gone)
			switch demand {
			case "subscriber":
				other := register()
				defer s.unregisterCIWatch(other)
			case "ci_hint":
				s.hintCIWatches(githubhook.Hint{RepositoryID: 801, PullNumbers: []int64{42}, CI: true})
			case "fallback":
				clock.Add(ciWatchFallbackInterval)
			}
			release()
			var got reply
			select {
			case got = <-done:
			case <-time.After(time.Second):
				t.Fatal("claim did not finish authorization")
			}
			if got.target != nil {
				defer got.target.watch.entry.cancel()
			}
			if got.err != nil || (got.target == nil) != (demand == "none") {
				t.Fatal("claim ignored demand changes during authorization", demand, got.target, got.err)
			}
			if got.target != nil {
				if err := f.e.observeCIWatch(t.Context(), *got.target, s); err != nil {
					t.Fatal(err)
				}
			} else if retained.entries[0].inflight || retained.entries[0].started != 1 {
				t.Fatal("departed demand reserved a collection")
			}
		})
	}
}

func TestCIWatchUnregisterPreservesCompletedObservation(t *testing.T) {
	f, s, _, reader, retained, register := ciWatchDemandFixture(t)
	gone := register()
	reader.states[42] = "failed"
	watchTick(t, f, s)
	got := s.ciWatchResult(gone)
	if got.State != "failed" || got.Targets[0].ObservedAt == "" || got.Targets[0].ObservationKey == "" {
		t.Fatal("fresh collection during the call was not visible", got)
	}
	before := s.ciWatchResult(retained)
	s.unregisterCIWatch(gone)
	if after := s.ciWatchResult(retained); !reflect.DeepEqual(after, before) || after.State != "failed" {
		t.Fatal("unregister discarded a completed observation", after, before)
	}
}
