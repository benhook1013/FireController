package reviewexec

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewEvidenceCoverageLimitsBoundsAndAbsentEncoding(t *testing.T) {
	_, _, original := evidenceFixture(t, "cli", true)
	cases := []struct {
		name       string
		limits     []string
		incomplete bool
		valid      bool
	}{
		{"absent", nil, false, true},
		{"empty", []string{}, false, true},
		{"sixteen", make([]string, 16), false, true},
		{"seventeen", make([]string, 17), false, false},
		{"byte-boundary", []string{strings.Repeat("a", 1024)}, false, true},
		{"too-long", []string{strings.Repeat("a", 1025)}, false, false},
		{"empty-text", []string{""}, false, false},
		{"blank-text", []string{" \t\n"}, false, false},
		{"control", []string{"unsafe\x00text"}, false, false},
		{"invalid-utf8", []string{string([]byte{0xff})}, false, false},
		{"incomplete-fifteen", make([]string, 15), true, true},
		{"incomplete-sixteen", make([]string, 16), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := original
			input.Review.CoverageLimits = append([]string(nil), tc.limits...)
			if len(tc.limits) > 1 {
				for i := range input.Review.CoverageLimits {
					input.Review.CoverageLimits[i] = "Reported coverage limitation"
				}
			}
			if tc.incomplete {
				input.Review.Complete, input.Review.Outcome = false, "failed"
			}
			if err := validateReviewEvidence(input); (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
		})
	}
	before, err := json.Marshal(original)
	if err != nil || strings.Contains(string(before), "coverage_limits") {
		t.Fatal("optional absent field changed legacy canonical input", err)
	}
	var decoded ReviewEvidenceInput
	if err := json.Unmarshal(before, &decoded); err != nil || validateReviewEvidence(decoded) != nil {
		t.Fatal("existing absent metadata stopped validating", err)
	}
	after, _ := json.Marshal(decoded)
	if string(after) != string(before) {
		t.Fatal("absent metadata changed retained replay bytes")
	}
}

func TestNativeCoverageLimitsSurvivePublicationDecisionsFinalizationAndFix(t *testing.T) {
	f, a, input := evidenceFixture(t, "cli", true)
	if err := EnsureAdjudicationSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	limits := []string{"CodeRabbit reported one file not fully reviewed because its changes were unavailable."}
	input.Review.CoverageLimits = append([]string(nil), limits...)
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	read := func() {
		t.Helper()
		evidence, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), receipt.ID)
		if err != nil || !reflect.DeepEqual(evidence.Capture.Review.CoverageLimits, limits) {
			t.Fatal("retained metadata changed", evidence, err)
		}
		if _, err := f.e.PublishEvidence(t.Context(), attemptProject, receipt.ID, cliPublicationPull(2)); err != nil {
			t.Fatal("republishing retained limitation conflicted with current decisions", err)
		}
		detail := cliPublicationDetail(t, f, 2, receipt.ID)
		if !reflect.DeepEqual(detail.Run.CoverageLimits, limits) || !detail.Run.Complete || detail.Run.Found != 1 {
			t.Fatal("publication lost limits or changed capture completion/counts", detail.Run)
		}
		public, err := json.Marshal(publicEvidence(evidence))
		if err != nil || strings.Contains(string(public), "coverage_limits") || strings.Contains(string(public), limits[0]) {
			t.Fatal("private parser metadata changed public evidence shape", err)
		}
	}
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, receipt.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	read()
	adjudicationDecide(t, f, receipt.ID, "coverage-accept", 0, 0, "accepted")
	read()
	adjudicationComplete(t, f, a)
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "coverage-finalize", receipt.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, receipt.ID, "finalized", 2)
	read()
	body, err = f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "coverage-fix", receipt.ID, FindingFixRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: "Retained fix proof"})
	adjudicationReceipt(t, body, err, receipt.ID, "fix_recorded", 3)
	read()
	final := cliPublicationDetail(t, f, 2, receipt.ID)
	state := adjudicationRead(t, f, receipt.ID)
	if !final.Run.Finalized || final.Run.Accepted != 1 || final.Run.Fixed != 1 || state.FrozenCounts == nil || *state.FrozenCounts != (FindingCounts{Found: 1, Accepted: 1}) {
		t.Fatal("coverage metadata changed finalized accounting or effective fix proof", final.Run, state)
	}
	replay, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != receipt {
		t.Fatal("metadata changed exact source replay", replay, err)
	}
	input.Review.CoverageLimits[0] = "Changed coverage limitation"
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input); err == nil {
		t.Fatal("changed coverage metadata rewrote immutable source")
	}
}

func TestIncompleteNativeCoverageLimitsRetainExistingWarning(t *testing.T) {
	_, _, input := evidenceFixture(t, "cli", true)
	input.Review.Complete, input.Review.Outcome = false, "failed"
	input.Review.CoverageLimits = []string{"Retained source limitation"}
	evidence := ReviewEvidence{ID: "review_evidence_" + strings.Repeat("a", 32), Capture: input, Anchor: input.ExpectedAnchor}
	state := ReviewAdjudication{Findings: []FindingDecision{{}}}
	run, err := publicationRun(evidence, state, Attempt{Provider: "coderabbit"})
	if err != nil || len(run.CoverageLimits) != 2 || run.CoverageLimits[0] != input.Review.CoverageLimits[0] || !strings.HasPrefix(run.CoverageLimits[1], "Incomplete CLI capture;") || run.Complete {
		t.Fatal("incomplete capture lost limitation/warning or gained completion", run, err)
	}
	run.CoverageLimits[0] = "Projection edit"
	if input.Review.CoverageLimits[0] != "Retained source limitation" {
		t.Fatal("projection aliases immutable source metadata")
	}
}
