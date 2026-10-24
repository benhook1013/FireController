package reviewexec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCommitDecoupledProgressKeepsCompletedNativeChannel(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var id string
			var current SourcePlan
			if channel == "cli" {
				f, a, id, current = coverageFixture(t, 1, false)
				adjudicationDecide(t, f, id, "accept-original", 0, 0, "accepted")
				coverageFinalize(t, f, a, id, 1)
				coverageFix(t, f, id, "fix-original", 2, 0, "")
			} else {
				f, a, id, current = hostedComparisonFixture(t, true)
				coverageFinalize(t, f, a, id, 0)
			}
			original, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
			if err != nil {
				t.Fatal(err)
			}
			before := adjudicationState(t, f, a)
			for name, move := range map[string]func(*SourcePlan){
				"unchanged": func(*SourcePlan) {},
				"candidate": func(p *SourcePlan) { p.Pull.HeadSHA = strings.Repeat("9", 40) },
				"base_and_parent": func(p *SourcePlan) {
					p.Pull.BaseSHA = strings.Repeat("8", 40)
					p.ParentSHA = p.Pull.BaseSHA
				},
				"retargeted_parent_and_ref": func(p *SourcePlan) {
					p.Pull.BaseRef, p.ParentPullNumber = "new-parent", 17
					p.Pull.BaseSHA, p.ParentSHA = strings.Repeat("7", 40), strings.Repeat("7", 40)
				},
				"rewritten_branch_history": func(p *SourcePlan) {
					p.Pull.HeadSHA, p.Pull.HeadRef = strings.Repeat("6", 40), "rewritten-candidate"
					p.Pull.BaseSHA, p.ParentSHA = strings.Repeat("5", 40), strings.Repeat("5", 40)
					p.Pull.BaseRef, p.ParentPullNumber = "rewritten-parent", 19
				},
			} {
				t.Run(name, func(t *testing.T) {
					moved := current
					move(&moved)
					got, err := f.e.Comparison(t.Context(), attemptProject, moved, channel)
					if err != nil || got != (ComparisonState{Covered: true}) {
						t.Fatal("source movement reopened completed PR/channel progress", got, err)
					}
					if done, err := f.e.DiscoveryComplete(t.Context(), attemptProject, moved, channel); err != nil || !done {
						t.Fatal("source movement forced another review", done, err)
					}
				})
			}
			retained, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
			if err != nil || !reflect.DeepEqual(retained, original) || original.Anchor != *a.Anchor || before != adjudicationState(t, f, a) {
				t.Fatal("progress read rewrote frozen admission/evidence", err)
			}
			// A later admitted intent in the other channel inherits no completed
			// credit from this channel, even for the same PR after a new commit.
			other := "hosted"
			if channel == "hosted" {
				other = "cli"
			}
			moved := current
			moved.Pull.HeadSHA = strings.Repeat("4", 40)
			later := coverageNextAttempt(t, f, "later-independent-channel", other, moved.Pull.HeadSHA)
			if later.Anchor == nil || later.Anchor.CandidateSHA != moved.Pull.HeadSHA {
				t.Fatal("later channel was not admitted against its actual candidate")
			}
			got, err := f.e.Comparison(t.Context(), attemptProject, moved, other)
			if err != nil || got != (ComparisonState{}) {
				t.Fatal("completed progress leaked across channels", got, err)
			}
			got, err = f.e.Comparison(t.Context(), attemptProject, moved, channel)
			if err != nil || got != (ComparisonState{Covered: true}) {
				t.Fatal("other channel admission erased completed progress", got, err)
			}
		})
	}
}

func TestCommitDecoupledProgressPreservesExhaustionAndCleanTaper(t *testing.T) {
	for _, test := range []struct {
		name    string
		maximum int64
		rounds  int
		tapered bool
	}{
		{"exhausted_without_taper", 1, 1, false},
		{"clean_taper_before_maximum", 5, 3, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := roundFixture(t)
			roundGrant(t, f, 2, "cli", 0, 0, test.maximum, "finite-grant")
			var a Attempt
			for i := 0; i < test.rounds; i++ {
				var id string
				a, id = roundCapture(t, f, i, false)
				roundFinalize(t, f, a, id, i, false)
			}
			control := roundRead(t, f, 2, "cli")
			if control.Used != int64(test.rounds) || control.Dry != int64(test.rounds) || control.Maximum != test.maximum || control.Tapered != test.tapered || !roundDone(control) {
				t.Fatal("fixture did not distinguish exhaustion and taper", control)
			}
			progress := readProgress(t, f, 2).Items[0]
			before := roundTables(t, f)
			moved := SourcePlan{Repository: reviewprovider.Repository{ID: a.Anchor.RepositoryID, FullName: a.Anchor.Repository, DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: a.Anchor.RepositoryID, State: "open", HeadSHA: strings.Repeat("9", 40), HeadRef: "rewritten-head", BaseSHA: strings.Repeat("8", 40), BaseRef: "different-parent", ChangedFiles: 2}, ParentSHA: strings.Repeat("8", 40), ParentPullNumber: 17, ObservedAt: stamp()}
			got, err := f.e.Comparison(t.Context(), attemptProject, moved, "cli")
			if err != nil || got != (ComparisonState{Covered: true, RoundsConfigured: true, RoundsComplete: true}) {
				t.Fatal("movement erased native completion or stopping state", got, err)
			}
			if done, err := f.e.DiscoveryComplete(t.Context(), attemptProject, moved, "cli"); err != nil || !done {
				t.Fatal("movement reopened finished discovery", done, err)
			}
			_, err = f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "movement-must-not-buy-round", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: moved.Pull.HeadSHA})
			attemptCode(t, err, "review_round_complete")
			if !reflect.DeepEqual(control, roundRead(t, f, 2, "cli")) || !reflect.DeepEqual(progress, readProgress(t, f, 2).Items[0]) || before != roundTables(t, f) {
				t.Fatal("movement changed counters, tapered state or grant history")
			}
		})
	}
}

func TestCommitDecoupledProgressPendingDominatesReadyAcrossMovement(t *testing.T) {
	for _, mode := range []string{"missing_same_head_result", "original_result_finalized_after_move"} {
		t.Run(mode, func(t *testing.T) {
			f, ready, readyID, current := coverageFixture(t, 0, false)
			coverageFinalize(t, f, ready, readyID, 0)
			if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			var unfinished Attempt
			var unfinishedID string
			if mode == "missing_same_head_result" {
				unfinished = comparisonCompleteMissingCapture(t, f, "old-same-head-missing-result", "cli", current.Pull.HeadSHA)
			} else {
				// This original-head result is retained before the source moves,
				// but deliberately remains unfinalized until after that movement.
				unfinished, unfinishedID = roundCapture(t, f, 901, true)
			}
			moved := current
			moved.Pull.HeadSHA = strings.Repeat("9", 40)
			moved.Pull.BaseSHA, moved.ParentSHA = strings.Repeat("8", 40), strings.Repeat("8", 40)
			moved.Pull.BaseRef, moved.ParentPullNumber = "new-parent", 17
			for _, plan := range []SourcePlan{current, moved} {
				coverageComparison(t, f, ready, plan, ComparisonState{Covered: true, Pending: true})
				if done, err := f.e.DiscoveryComplete(t.Context(), attemptProject, plan, "cli"); err != nil || done {
					t.Fatal("ready result hid unresolved original work", done, err)
				}
			}
			if unfinishedID == "" {
				return
			}
			roundFinalize(t, f, unfinished, unfinishedID, 901, true)
			// Frozen accepted counts alone never clear the unfixed finding.
			coverageComparison(t, f, ready, moved, ComparisonState{Covered: true, Pending: true})
			coverageFix(t, f, unfinishedID, "fix-original-after-move", 2, 0, "")
			coverageComparison(t, f, ready, moved, ComparisonState{Covered: true})
			retained, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), unfinishedID)
			counts := adjudicationRead(t, f, unfinishedID)
			if err != nil || retained.Anchor != *unfinished.Anchor || retained.Anchor.CandidateSHA != current.Pull.HeadSHA || counts.FrozenCounts == nil || *counts.FrozenCounts != (FindingCounts{Found: 1, Accepted: 1}) {
				t.Fatal("late finalization changed original anchor or frozen source counts", err)
			}
		})
	}
}
