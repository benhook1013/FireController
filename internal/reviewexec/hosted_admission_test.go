package reviewexec

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// These tests exercise the final local guard only. No provider allowance or
// posting authority is supplied or inferred by a successful verification.
func hostedAdmissionNoHTTP(t *testing.T) {
	t.Helper()
	trap := &cliPublicationHTTPTrap{}
	transport, client := http.DefaultTransport, http.DefaultClient
	http.DefaultTransport, http.DefaultClient = trap, &http.Client{Transport: trap}
	t.Cleanup(func() {
		http.DefaultTransport, http.DefaultClient = transport, client
		if trap.calls.Load() != 0 {
			t.Fatal("local admission contacted a provider")
		}
	})
}

func hostedAdmissionAttempt(t *testing.T, f *attemptFixture, key, channel string, configuration int64, anchor Anchor) Attempt {
	t.Helper()
	body, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), key, Request{ExpectedConfigurationRevision: configuration, PullNumber: anchor.PullNumber, Channel: channel, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	a := f.start(t, attemptProject, receipt)
	intent := ""
	if channel == "hosted" {
		intent, err = reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func hostedAdmissionVerify(t *testing.T, f *attemptFixture, a Attempt, code, reason string, plans ...SourcePlan) {
	t.Helper()
	before := adjudicationState(t, f, a)
	err := f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision, plans...)
	if code == "" {
		if err != nil {
			t.Fatal("valid local guard refused", err)
		}
	} else {
		attemptCode(t, err, code)
	}
	if reason != "" {
		var failure *core.Error
		if !errors.As(err, &failure) {
			t.Fatal("missing action hold error", err)
		}
		details, ok := failure.Details.(map[string]string)
		if !ok || details["reason"] != reason {
			t.Fatal("wrong action hold reason", err)
		}
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("final verification mutated journal, evidence, events or receipts")
	}
}

func TestHostedAdmissionTargetRequiresFinalizedCurrentFixProof(t *testing.T) {
	hostedAdmissionNoHTTP(t)
	f, prior, id, _ := hostedComparisonFixture(t, false)
	adjudicationComplete(t, f, prior)
	next := hostedAdmissionAttempt(t, f, "hosted-current-action", "hosted", 1, *prior.Anchor)
	hostedAdmissionVerify(t, f, next, "review_evidence_pending", "")
	adjudicationDecide(t, f, id, "hosted-admission-accept", 0, 0, "accepted")
	adjudicationDecide(t, f, id, "hosted-admission-reject-one", 1, 1, "rejected")
	adjudicationDecide(t, f, id, "hosted-admission-reject-two", 2, 2, "rejected")
	hostedAdmissionVerify(t, f, next, "review_evidence_pending", "")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-admission-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(3)})
	adjudicationReceipt(t, body, err, id, "finalized", 4)
	hostedAdmissionVerify(t, f, next, "review_evidence_pending", "")
	coverageFix(t, f, id, "hosted-admission-fix", 4, 0, "")
	hostedAdmissionVerify(t, f, next, "review_held", "target_already_covered")
}

func TestHostedAdmissionOldHeadPendingAndLateCorrection(t *testing.T) {
	hostedAdmissionNoHTTP(t)
	f, prior, id, _ := hostedComparisonFixture(t, false)
	adjudicationComplete(t, f, prior)
	anchor := *prior.Anchor
	anchor.CandidateSHA = strings.Repeat("9", 40)
	next := hostedAdmissionAttempt(t, f, "hosted-moved-action", "hosted", 1, anchor)
	hostedAdmissionVerify(t, f, next, "review_evidence_pending", "")
	for i := int64(0); i < 3; i++ {
		adjudicationDecide(t, f, id, "hosted-close-"+string(rune('a'+i)), i, i, "rejected")
	}
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-close-old", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(3)})
	adjudicationReceipt(t, body, err, id, "finalized", 4)
	hostedAdmissionVerify(t, f, next, "", "")
	adjudicationDecide(t, f, id, "hosted-old-correction", 4, 0, "accepted")
	hostedAdmissionVerify(t, f, next, "review_evidence_pending", "")
	coverageFix(t, f, id, "hosted-old-fix", 5, 0, "")
	hostedAdmissionVerify(t, f, next, "", "")
	// Missing capture on an older completed intent remains a selected-channel hold.
	coverageSeedOperations(t, f, prior, 1, "completed")
	hostedAdmissionVerify(t, f, next, "review_evidence_pending", "")
}

func TestHostedAdmissionChannelsDoNotShareObligations(t *testing.T) {
	for _, history := range []string{"cli", "hosted"} {
		t.Run(history, func(t *testing.T) {
			hostedAdmissionNoHTTP(t)
			var f *attemptFixture
			var prior Attempt
			other := "cli"
			if history == "cli" {
				f, prior, _, _ = coverageFixture(t, 1, false)
				other = "hosted"
			} else {
				f, prior, _, _ = hostedComparisonFixture(t, false)
			}
			adjudicationComplete(t, f, prior)
			next := hostedAdmissionAttempt(t, f, "other-channel-action", other, 1, *prior.Anchor)
			hostedAdmissionVerify(t, f, next, "", "")
		})
	}
}

func hostedAdmissionStack(t *testing.T, f *attemptFixture, open SourcePlan) (Attempt, []SourcePlan) {
	t.Helper()
	previous := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "hosted-stack-config", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 3, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	anchor := attemptAnchor(attemptProject, 4)
	anchor.ParentPullNumber, anchor.ParentSHA, anchor.BaseSHA, anchor.BaseRef = 2, open.Pull.HeadSHA, open.Pull.HeadSHA, "parent-branch"
	child := hostedAdmissionAttempt(t, f, "hosted-stack-child", "hosted", 2, anchor)
	merged := open
	merged.Pull.Number, merged.Pull.Merged, merged.Pull.State = 3, true, "closed"
	merged.ParentSHA, merged.ParentPullNumber = "", 0
	return child, []SourcePlan{open, merged}
}

func TestHostedAdmissionStackRequiresHostedPredecessor(t *testing.T) {
	hostedAdmissionNoHTTP(t)
	f, prior, id, open := coverageFixture(t, 0, false)
	coverageFinalize(t, f, prior, id, 0)
	child, plans := hostedAdmissionStack(t, f, open)
	// A complete native CLI predecessor cannot advance the hosted cycle.
	hostedAdmissionVerify(t, f, child, "review_held", "predecessor_unfinished", plans...)
}

func TestHostedAdmissionStackChecksAllFreshPlansAndLateFixCorrection(t *testing.T) {
	hostedAdmissionNoHTTP(t)
	f, prior, id, open := hostedComparisonFixture(t, false)
	for i := int64(0); i < 3; i++ {
		adjudicationDecide(t, f, id, "hosted-stack-reject-"+string(rune('a'+i)), i, i, "rejected")
	}
	coverageFinalize(t, f, prior, id, 3)
	child, plans := hostedAdmissionStack(t, f, open)
	hostedAdmissionVerify(t, f, child, "", "", plans...)
	cases := map[string]func([]SourcePlan) []SourcePlan{
		"missing":             func(p []SourcePlan) []SourcePlan { return p[:1] },
		"extra":               func(p []SourcePlan) []SourcePlan { return append(p, p[0]) },
		"reordered":           func(p []SourcePlan) []SourcePlan { return []SourcePlan{p[1], p[0]} },
		"foreign":             func(p []SourcePlan) []SourcePlan { p[0].Repository.ID++; return p },
		"closed unmerged":     func(p []SourcePlan) []SourcePlan { p[1].Pull.Merged = false; return p },
		"bad merged head":     func(p []SourcePlan) []SourcePlan { p[1].Pull.HeadSHA = "bad"; return p },
		"missing observation": func(p []SourcePlan) []SourcePlan { p[1].ObservedAt = ""; return p },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			hostedAdmissionVerify(t, f, child, "validation_failed", "", change(append([]SourcePlan(nil), plans...))...)
		})
	}
	moved := append([]SourcePlan(nil), plans...)
	moved[0].Pull.HeadSHA = strings.Repeat("9", 40)
	hostedAdmissionVerify(t, f, child, "review_held", "predecessor_unfinished", moved...)
	adjudicationDecide(t, f, id, "hosted-stack-correct", 4, 0, "accepted")
	hostedAdmissionVerify(t, f, child, "review_held", "predecessor_unfinished", plans...)
	coverageFix(t, f, id, "hosted-stack-fix", 5, 0, "")
	hostedAdmissionVerify(t, f, child, "", "", plans...)
	admissionPolicy(t, f, 1, "hosted")
	hostedAdmissionVerify(t, f, child, "review_conflict", "", plans...)
}
