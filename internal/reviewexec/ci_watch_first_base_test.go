package reviewexec

import (
	"strings"
	"testing"
)

// Deliberately defer every subscriber read until two real observations have
// completed. Notifications do not schedule reads in this deterministic test.
func TestCIWatchFirstBasePinnedBeforeDelayedSubscriberRead(t *testing.T) {
	f, s, clock, reader, scope := ciWatchFixture(t)
	head, first, second, otherHead := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	binding, err := f.e.ciWatchBinding(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	register := func(target CIWatchTarget) *ciWatchSubscription {
		t.Helper()
		sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{target})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.unregisterCIWatch(sub) })
		return sub
	}
	implicit := CIWatchTarget{PullNumber: 42, HeadSHA: head}
	early := register(implicit)
	explicit := register(CIWatchTarget{PullNumber: 42, HeadSHA: head, BaseSHA: second})
	wrongHead := register(CIWatchTarget{PullNumber: 42, HeadSHA: otherHead})
	reader.states[42] = "satisfied"
	claim, err := f.e.claimLifecycleLane(t.Context(), s.readers, s, lifecycleEnrichment)
	if err != nil || claim == nil || claim.watch == nil {
		t.Fatal(claim, err)
	}
	// This owner arrived after the claim started, so B1 cannot pin its base.
	during := register(implicit)
	if err = f.e.observeLifecycle(t.Context(), *claim, s); err != nil {
		t.Fatal(err)
	}
	if early.bases[0] != first || explicit.bases[0] != second || during.bases[0] != "" || wrongHead.bases[0] != "" {
		t.Fatal("first completion pinned wrong subscribers", early.bases, explicit.bases, during.bases, wrongHead.bases)
	}
	// Nor can a cached B1 observation pin a later subscriber.
	after := register(implicit)
	if got := s.ciWatchResult(after); got.Targets[0].ObservedBaseSHA != "" || after.bases[0] != "" {
		t.Fatal("cached observation pinned later subscriber", got)
	}
	pull := lifecyclePull(42)
	pull.BaseSHA = second
	pull.BaseRef = "stack/parent"
	reader.pulls[42] = pull
	clock.Add(ciWatchFallbackInterval)
	watchTick(t, f, s)
	for _, tc := range []struct {
		name        string
		sub         *ciWatchSubscription
		state, base string
	}{
		{"early", early, "superseded", first}, {"explicit", explicit, "passed", second}, {"during", during, "passed", second}, {"after", after, "passed", second}, {"wrong head", wrongHead, "superseded", ""},
	} {
		got := s.ciWatchResult(tc.sub)
		if got.State != tc.state || got.Targets[0].State != tc.state || got.Targets[0].ObservedBaseSHA != second || tc.sub.bases[0] != tc.base {
			t.Fatal(tc.name, "changed original comparison or adopted ineligible base", got, tc.sub.bases)
		}
	}
}

func TestCIWatchFirstBaseIgnoresUnprovenSource(t *testing.T) {
	for _, badBase := range []string{"", "short"} {
		t.Run(badBase, func(t *testing.T) {
			f, s, clock, reader, scope := ciWatchFixture(t)
			binding, err := f.e.ciWatchBinding(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.unregisterCIWatch(sub)
			pull := lifecyclePull(42)
			pull.BaseSHA = badBase
			pull.BaseRef = "stack/parent"
			reader.pulls[42] = pull
			watchTick(t, f, s)
			if sub.bases[0] != "" {
				t.Fatal("unproven source pinned base", sub.bases)
			}
			pull.BaseSHA = strings.Repeat("c", 40)
			reader.pulls[42] = pull
			reader.states[42] = "satisfied"
			clock.Add(ciWatchFallbackInterval)
			watchTick(t, f, s)
			if sub.bases[0] != pull.BaseSHA || s.ciWatchResult(sub).State != "passed" {
				t.Fatal("first proven source was not pinned", sub.bases)
			}
		})
	}
}
