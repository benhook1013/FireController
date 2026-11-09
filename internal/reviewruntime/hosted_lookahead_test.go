package reviewruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestHostedLookaheadSourceKeepsPendingAncestorAndExactParent(t *testing.T) {
	f := newCLIDriverFixture(t)
	seedSourceHostedEvidence(t, f, false)
	parent := f.reader.pulls[2]
	parent.HeadRef = "lookahead-parent"
	f.reader.pulls[2] = parent
	child := f.reader.pulls[4]
	child.HeadSHA, child.BaseSHA, child.BaseRef = strings.Repeat("9", 40), parent.HeadSHA, parent.HeadRef
	f.reader.pulls[4] = child
	configuration := sourceConfiguration("stacked", 2, 4)
	operation := sourceOperation(f, 4, "hosted")
	before := sourceState(t, f)
	plan, parents, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, configuration, operation)
	if err != nil || plan.ParentPullNumber != 2 || plan.ParentSHA != parent.HeadSHA || len(parents) != 1 || parents[0].Pull.Number != 2 {
		t.Fatal("Hosted look-ahead lost the exact pending ancestor", plan, parents, err)
	}
	_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, configuration, sourceOperation(f, 2, "hosted"))
	assertCoreErrorCode(t, err, "review_evidence_pending")
	_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, configuration, sourceOperation(f, 4, "cli"))
	sourceHoldReason(t, err, "predecessor_unfinished")
	parent.HeadSHA = strings.Repeat("8", 40)
	f.reader.pulls[2] = parent
	_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, configuration, operation)
	sourceHoldReason(t, err, "parent_moved")
	if sourceState(t, f) != before || f.fetches.Load() != 0 {
		t.Fatal("source look-ahead changed evidence or materialized work")
	}
}

func TestHostedLookaheadDriverPostsOnlyFreshNamedParentComparison(t *testing.T) {
	for _, moveParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable", true: "parent-moved"}[moveParent], func(t *testing.T) {
			f := newHostedDriverFixture(t)
			seedSourceHostedEvidence(t, f.cliDriverFixture, false)
			parent := f.reader.pulls[2]
			parent.HeadSHA, parent.HeadRef = f.baseSHA, "lookahead-parent"
			f.reader.pulls[2] = parent
			child := f.reader.pulls[4]
			child.BaseSHA, child.BaseRef = parent.HeadSHA, parent.HeadRef
			f.reader.pulls[4] = child
			f.configure(t, "stacked", []int64{2, 4})
			f.provider.pullNumber = 4
			raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "lookahead-driver", reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "hosted", HeadSHA: child.HeadSHA})
			if err != nil {
				t.Fatal(err)
			}
			var receipt reviewexec.Receipt
			if err = json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			attempt, err := f.engine.StartAttempt(t.Context(), f.project, receipt.ID)
			if err != nil {
				t.Fatal(err)
			}
			current, err := f.engine.ExecutionContext(t.Context(), f.project, receipt.ID)
			if err != nil || current.Operation.AllowOutOfOrder {
				t.Fatal("ordinary intent changed", current, err)
			}
			prepared, err := f.hosted.Prepare(t.Context(), current.Operation, current.Configuration, attempt)
			if err != nil || prepared.Anchor.ParentPullNumber != 2 || prepared.Anchor.ParentSHA != parent.HeadSHA {
				t.Fatal("look-ahead source preparation", prepared, err)
			}
			attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, prepared.Anchor, prepared.Intent)
			if err != nil {
				t.Fatal(err)
			}
			if moveParent {
				parent.HeadSHA = strings.Repeat("9", 40)
				f.reader.pulls[2] = parent
			}
			result, err := f.hosted.Dispatch(t.Context(), current.Operation, attempt, f.callback(t, &attempt))
			if err != nil {
				t.Fatal(err)
			}
			if moveParent {
				if result.State != "failed" || f.provider.reviews != 0 {
					t.Fatal("stale comparison reached provider", result, f.provider.reviews)
				}
			} else if result.State != "completed" || f.provider.reviews != 1 {
				t.Fatal("ordinary Hosted child did not complete", result, f.provider.reviews)
			}
			if f.reader.calls[3] != 0 {
				t.Fatal("look-ahead read an unrelated PR")
			}
		})
	}
}
