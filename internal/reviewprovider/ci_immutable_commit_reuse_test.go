package reviewprovider

import (
	"context"
	"testing"
)

func TestCIObservationReusesOnlyImmutableMergeObject(t *testing.T) {
	f := newCIObservationFixture(t, "")
	for i := 1; i <= 2; i++ {
		got, err := f.g.ObserveRequiredCI(context.Background(), ciObservationTarget())
		if err != nil || got.State != "satisfied" {
			t.Fatalf("observation: %s %v", got.State, err)
		}
		_, counts := f.snapshot()
		if counts[candidateCommitPath] != i {
			t.Fatalf("immutable reads = %d, want %d", counts[candidateCommitPath], i)
		}
		if counts[candidatePullPath] != 4*i || counts[candidateBranchPath] != 4*i {
			t.Fatalf("mutable brackets weakened: %v", counts)
		}
	}
}

func TestCIImmutableReuseStillRejectsMovedCandidate(t *testing.T) {
	f := newCIObservationFixture(t, "move_candidate")
	got, err := f.g.ObserveRequiredCI(context.Background(), ciObservationTarget())
	if err != nil || got.State == "satisfied" {
		t.Fatalf("movement accepted: %s %v", got.State, err)
	}
}
