package reviewexec

import (
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"reflect"
	"strings"
	"testing"
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
					if done, err := f.e.Coverage(t.Context(), attemptProject, moved, channel); err != nil || !done {
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

func TestCommitDecoupledProgressPendingDominatesReadyAcrossMovement(t *testing.T) {
	for _, mode := range []string{"missing_same_head_result", "original_result_finalized_after_move"} {
		t.Run(mode, func(t *testing.T) {
			f, ready, readyID, current := coverageFixture(t, 0, false)
			coverageFinalize(t, f, ready, readyID, 0)
			var unfinished Attempt
			var unfinishedID string
			if mode == "missing_same_head_result" {
				unfinished = comparisonCompleteMissingCapture(t, f, "old-same-head-missing-result", "cli", current.Pull.HeadSHA)
			} else {
				// This original-head result is retained before the source moves,
				// but deliberately remains unfinalized until after that movement.
				unfinished = coverageNextAttempt(t, f, "original-unfinished", "cli", current.Pull.HeadSHA)
				unfinishedID = completionRetainFinding(t, f, readyID, unfinished)
				adjudicationComplete(t, f, unfinished)
			}
			moved := current
			moved.Pull.HeadSHA = strings.Repeat("9", 40)
			moved.Pull.BaseSHA, moved.ParentSHA = strings.Repeat("8", 40), strings.Repeat("8", 40)
			moved.Pull.BaseRef, moved.ParentPullNumber = "new-parent", 17
			for _, plan := range []SourcePlan{current, moved} {
				coverageComparison(t, f, ready, plan, ComparisonState{Covered: true, Pending: true})
				if done, err := f.e.Coverage(t.Context(), attemptProject, plan, "cli"); err != nil || done {
					t.Fatal("ready result hid unresolved original work", done, err)
				}
			}
			if unfinishedID == "" {
				return
			}
			adjudicationDecide(t, f, unfinishedID, "accept-original-after-move", 0, 0, "accepted")
			body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "finalize-original-after-move", unfinishedID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
			adjudicationReceipt(t, body, err, unfinishedID, "finalized", 2)
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

// Reuse the canonical retained capture structure, preserving native attempt identity.
func completionRetainFinding(t *testing.T, f *attemptFixture, originalID string, next Attempt) string {
	t.Helper()
	original, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), originalID)
	if err != nil {
		t.Fatal(err)
	}
	capture := original.Capture
	capture.ExpectedAnchor = *next.Anchor
	capture.Review.Findings = []reviewprovider.ParsedFinding{{Severity: "high", Title: "Original retained obligation", Detail: "Focused original finding", Path: "main.go", StartLine: 1, EndLine: 1}}
	capture.Review.ReportedFindings = 1
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	return receipt.ID
}
