package reviewexec

import (
	"context"
	"database/sql"
	"testing"
)

// A parent's source hold, not unfinished review rounds, is an admission barrier
// to the child's pinned diff. Preserve the expected child's own diagnosis.
func TestExpectedFrontRetainsPredecessorSourceHold(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2, 4})
			parent, child := laneDiagnosticPlan(2), laneDiagnosticPlan(4)
			parent.SourceReason = "pull_draft"
			child.ParentPullNumber, child.ParentSHA, child.Pull.BaseSHA = 2, parent.Pull.HeadSHA, parent.Pull.HeadSHA
			r := Run{ProjectID: attemptProject, Channel: channel, ExpectedPullNumber: 4}
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				got, err := f.e.evaluateFront(ctx, tx, r, 801, []SourcePlan{parent, child}, true)
				if err == nil && (got.reason != "predecessor_unfinished" || !got.holdsPull(4) || got.blockingPull != 2 || got.blockingReason != "pull_draft") {
					t.Fatalf("expected child's own predecessor source hold, got %+v", got)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if n := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`); n != 0 {
				t.Fatal("diagnosis dispatched review", n)
			}
		})
	}
}

// The terminal transaction uses both the retained reason and this ownership
// predicate. An identically worded hold on another PR cannot validate it.
func TestExpectedHoldOwnershipRejectsOtherTarget(t *testing.T) {
	p := laneDiagnosticPlan(4)
	for _, test := range []struct {
		name   string
		front  evaluatedFront
		number int64
		want   bool
	}{
		{"same", evaluatedFront{state: "held", reason: "predecessor_unfinished", plan: &p}, 4, true},
		{"other", evaluatedFront{state: "held", reason: "predecessor_unfinished", plan: &p}, 2, false},
		{"ready", evaluatedFront{state: "ready", plan: &p}, 4, false},
		{"missing", evaluatedFront{state: "held", reason: "predecessor_unfinished"}, 4, false},
		{"unasserted", evaluatedFront{state: "held", plan: &p}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.front.holdsPull(test.number); got != test.want {
				t.Fatalf("holdsPull=%v, want %v", got, test.want)
			}
		})
	}
}
