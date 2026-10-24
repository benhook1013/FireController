package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"testing"
)

type applicabilityTestVerifier struct {
	calls  int
	verify func(context.Context, Anchor, SourcePlan) (EquivalentComparison, error)
}

func (v *applicabilityTestVerifier) VerifyReviewApplicability(ctx context.Context, a Anchor, p SourcePlan) (EquivalentComparison, error) {
	v.calls++
	return v.verify(ctx, a, p)
}
func applicabilityTrees(a Anchor, p SourcePlan) EquivalentComparison {
	return EquivalentComparison{OriginalBaseTree: strings.Repeat("1", 40), CurrentBaseTree: strings.Repeat("1", 40), OriginalHeadTree: strings.Repeat("2", 40), CurrentHeadTree: strings.Repeat("2", 40), OriginalMergeBaseSHA: a.MergeBaseSHA, CurrentMergeBaseSHA: p.Pull.BaseSHA, PatchSHA256: a.PatchSHA256, InventorySHA256: strings.Repeat("3", 64)}
}
func applicabilityMoved(p SourcePlan) SourcePlan {
	p.Pull.HeadSHA = strings.Repeat("8", 40)
	p.Pull.BaseSHA = strings.Repeat("9", 40)
	p.ParentSHA = p.Pull.BaseSHA
	return p
}
func applicabilityRetain(t *testing.T, f *attemptFixture, p SourcePlan, proofs []ReviewApplicability) {
	t.Helper()
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return f.e.retainReviewApplicability(ctx, tx, attemptProject, p, proofs)
	}); err != nil {
		t.Fatal(err)
	}
}
func applicabilityValidate(t *testing.T, f *attemptFixture) {
	t.Helper()
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error { return ValidateReviewApplicability(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
}

func TestReviewApplicabilityEquivalentHistoryPreservesOriginalEvidence(t *testing.T) {
	f, a, id, original := coverageFixture(t, 1, false)
	adjudicationDecide(t, f, id, "reject", 0, 0, "rejected")
	coverageFinalize(t, f, a, id, 1)
	current := applicabilityMoved(original)
	baseline := adjudicationState(t, f, a)
	v := &applicabilityTestVerifier{verify: func(ctx context.Context, anchor Anchor, p SourcePlan) (EquivalentComparison, error) {
		// Git work runs without owning the database writer.
		if err := f.store.BackgroundWrite(ctx, func(context.Context, *sql.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		return applicabilityTrees(anchor, p), nil
	}}
	if err := f.e.RegisterApplicabilityVerifier("test-capability", v); err != nil {
		t.Fatal(err)
	}
	coverageRead(t, f, a, current, false)
	proofs := f.e.prepareReviewApplicability(t.Context(), attemptProject, current, "test-capability")
	if len(proofs) != 1 || proofs[0].EvidenceID != id || v.calls != 1 {
		t.Fatal("missing pinned proof", proofs, v.calls)
	}
	applicabilityRetain(t, f, current, proofs)
	applicabilityValidate(t, f)
	coverageRead(t, f, a, current, true)
	if adjudicationState(t, f, a) != baseline {
		t.Fatal("carry changed original review/round/transition state")
	}
	applicabilityRetain(t, f, current, proofs)
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_applicability`); n != 1 {
		t.Fatal("duplicate attestation", n)
	}
	if got := f.e.prepareReviewApplicability(t.Context(), attemptProject, current, "test-capability"); len(got) != 0 || v.calls != 1 {
		t.Fatal("already-proven target repeated Git work")
	}
	// Current findings remain authoritative even after equivalence was retained.
	adjudicationDecide(t, f, id, "correct-to-accepted", 2, 0, "accepted")
	coverageRead(t, f, a, current, false)
	applicabilityValidate(t, f)
	coverageFix(t, f, id, "fixed", 3, 0, "")
	coverageRead(t, f, a, current, true)
	for _, mutate := range []func(*SourcePlan){func(p *SourcePlan) { p.Pull.HeadSHA = strings.Repeat("7", 40) }, func(p *SourcePlan) { p.Pull.BaseSHA = strings.Repeat("6", 40); p.ParentSHA = p.Pull.BaseSHA }, func(p *SourcePlan) { p.ParentPullNumber = 4 }, func(p *SourcePlan) { p.Pull.BaseRef = "newmain"; p.Repository.DefaultBranch = "newmain" }, func(p *SourcePlan) { p.Pull.Number = 4 }} {
		moved := current
		mutate(&moved)
		coverageRead(t, f, a, moved, false)
	}
	if got, err := f.e.Coverage(t.Context(), attemptProject, current, "hosted"); err != nil || got {
		t.Fatal("cross-channel carry", got, err)
	}
	// Restart retains applicability without acquiring another review or fetching.
	reloaded, err := New(f.store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reloaded.Coverage(t.Context(), attemptProject, current, "cli"); err != nil || !got {
		t.Fatal("restart lost carry", got, err)
	}
}

func TestReviewApplicabilityRefusesUnprovenAndConcurrentCorrections(t *testing.T) {
	for _, name := range []string{"verifier-error", "base-tree", "head-tree", "patch", "merge-base", "inventory", "pending-correction"} {
		t.Run(name, func(t *testing.T) {
			f, a, id, original := coverageFixture(t, 1, false)
			adjudicationDecide(t, f, id, "reject", 0, 0, "rejected")
			coverageFinalize(t, f, a, id, 1)
			current := applicabilityMoved(original)
			v := &applicabilityTestVerifier{verify: func(_ context.Context, anchor Anchor, p SourcePlan) (EquivalentComparison, error) {
				proof := applicabilityTrees(anchor, p)
				switch name {
				case "verifier-error":
					return proof, errors.New("missing objects")
				case "base-tree":
					proof.CurrentBaseTree = strings.Repeat("4", 40)
				case "head-tree":
					proof.CurrentHeadTree = strings.Repeat("5", 40)
				case "patch":
					proof.PatchSHA256 = strings.Repeat("6", 64)
				case "merge-base":
					proof.OriginalMergeBaseSHA = strings.Repeat("7", 40)
				case "inventory":
					proof.InventorySHA256 = "invalid"
				}
				return proof, nil
			}}
			if err := f.e.RegisterApplicabilityVerifier("test-capability", v); err != nil {
				t.Fatal(err)
			}
			proofs := f.e.prepareReviewApplicability(t.Context(), attemptProject, current, "test-capability")
			if name == "pending-correction" {
				if len(proofs) != 1 {
					t.Fatal(proofs)
				}
				adjudicationDecide(t, f, id, "correct", 2, 0, "accepted")
			} else if len(proofs) != 0 {
				t.Fatal("unproven carry", proofs)
			}
			applicabilityRetain(t, f, current, proofs)
			coverageRead(t, f, a, current, false)
			if n := attemptCount(t, f.store, `SELECT count(*) FROM review_applicability`); n != 0 {
				t.Fatal("unready proof persisted", n)
			}
		})
	}
}

func TestReviewApplicabilitySourceRefreshRegistersWithoutReplacingReader(t *testing.T) {
	f, a, id, original := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	current := applicabilityMoved(original)
	v := &applicabilityTestVerifier{verify: func(_ context.Context, a Anchor, p SourcePlan) (EquivalentComparison, error) {
		return applicabilityTrees(a, p), nil
	}}
	if err := f.e.RegisterApplicabilityVerifier("test-capability", v); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RegisterApplicabilityVerifier("test-capability", v); err == nil {
		t.Fatal("replacement allowed")
	}
	if err := f.e.RegisterApplicabilityVerifier("foreign", v); err == nil {
		t.Fatal("foreign capability")
	}
	reader := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return current.Pull, nil }}
	request := refreshRequest()
	request.HeadSHA = current.Pull.HeadSHA
	receipt := refreshEnqueue(t, f, "equivalence-refresh", request)
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	coverageRead(t, f, a, current, true)
	applicabilityValidate(t, f)
}

func TestReviewApplicabilityHostedKeepsOriginalTriggerAndCounts(t *testing.T) {
	f, a, id, original := hostedComparisonFixture(t, true)
	adjudicationComplete(t, f, a)
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-finalize-applicability", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, body, err, id, "finalized", 1)
	current := applicabilityMoved(original)
	before := adjudicationState(t, f, a)
	v := &applicabilityTestVerifier{verify: func(_ context.Context, a Anchor, p SourcePlan) (EquivalentComparison, error) {
		return applicabilityTrees(a, p), nil
	}}
	if err := f.e.RegisterApplicabilityVerifier("test-capability", v); err != nil {
		t.Fatal(err)
	}
	proofs := f.e.prepareReviewApplicability(t.Context(), attemptProject, current, "test-capability")
	if len(proofs) != 1 || proofs[0].Channel != "hosted" {
		t.Fatal(proofs)
	}
	applicabilityRetain(t, f, current, proofs)
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true})
	coverageComparison(t, f, a, current, ComparisonState{})
	applicabilityValidate(t, f)
	if before != adjudicationState(t, f, a) {
		t.Fatal("modified original hosted capture or accounting")
	}
}
