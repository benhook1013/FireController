package reviewexec

import (
	"fmt"
	"strings"
	"testing"
)

// These are genuine distinct local journal intents, not raw inserted completed
// operations or forged derived ready rows. No provider action is dispatched and
// absent captures grant only pending obligations, never review credit.
func comparisonCompleteMissingCapture(t *testing.T, f *attemptFixture, key, channel, head string) Attempt {
	t.Helper()
	a := coverageNextAttempt(t, f, key, channel, head)
	if a.State == "posting" {
		var err error
		a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "synthetic_posted", "101")
		if err != nil {
			t.Fatal(err)
		}
	}
	completed, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "synthetic_missing_capture", "")
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func comparisonMissingHistory(t *testing.T, f *attemptFixture, count int, channel, head string) {
	t.Helper()
	// This is proof of the native journal comparison, not removal of the separate
	// stored browser workspace caps. Do not install its optional display projector.
	var err error
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		comparisonCompleteMissingCapture(t, f, fmt.Sprintf("growth-%s-missing-%d", channel, i), channel, head)
	}
}

func TestComparisonGrowthOlderPendingAndExactReadyCoexist(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var evidence string
			var current SourcePlan
			if channel == "cli" {
				f, a, evidence, current = coverageFixture(t, 0, false)
			} else {
				f, a, evidence, current = hostedComparisonFixture(t, true)
			}
			coverageFinalize(t, f, a, evidence, 0)
			// One authentic finalized current capture plus 102 completed older intents
			// exceeds the removed all-head ceiling. Missing older captures still hold.
			comparisonMissingHistory(t, f, 102, channel, strings.Repeat("9", 40))
			before := adjudicationState(t, f, a)
			state, err := f.e.Comparison(t.Context(), attemptProject, current, channel)
			if err != nil || state != (ComparisonState{Covered: true, Pending: true}) {
				t.Fatal("old pending history lost ready current comparison", state, err)
			}
			usable, err := f.e.Coverage(t.Context(), attemptProject, current, channel)
			if err != nil || usable {
				t.Fatal("pending history became usable coverage", usable, err)
			}
			var count int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE project_id=? AND pull_number=2 AND channel=? AND state='completed'`, attemptProject, channel).Scan(&count); err != nil || count != 103 {
				t.Fatal("growth proof did not cross lifetime boundary", count, err)
			}
			for name, change := range map[string]func(*SourcePlan){
				"head":            func(p *SourcePlan) { p.Pull.HeadSHA = strings.Repeat("8", 40) },
				"base-and-parent": func(p *SourcePlan) { p.Pull.BaseSHA = strings.Repeat("7", 40); p.ParentSHA = p.Pull.BaseSHA },
				"parent-number":   func(p *SourcePlan) { p.ParentPullNumber = 1 },
				"base-ref":        func(p *SourcePlan) { p.Pull.BaseRef = "other-parent"; p.ParentPullNumber = 1 },
			} {
				t.Run(name, func(t *testing.T) {
					plan := current
					change(&plan)
					got, err := f.e.Comparison(t.Context(), attemptProject, plan, channel)
					if err != nil || got != (ComparisonState{Pending: true}) {
						t.Fatal("growth weakened exact ready anchor", got, err)
					}
				})
			}
			other := "hosted"
			if channel == "hosted" {
				other = "cli"
			}
			got, err := f.e.Comparison(t.Context(), attemptProject, current, other)
			if err != nil || got != (ComparisonState{}) {
				t.Fatal("history leaked across channels", got, err)
			}
			if adjudicationState(t, f, a) != before {
				t.Fatal("growth reads mutated native authority")
			}
			if err := ValidateComparisonProofs(t.Context(), f.store.DB()); err != nil {
				t.Fatal("supported history produced inconsistent derived proof", err)
			}
			assertComparisonGrowthQueryPlans(t, f, current)
		})
	}
}

func assertComparisonGrowthQueryPlans(t *testing.T, f *attemptFixture, current SourcePlan) {
	t.Helper()
	for _, channel := range []string{"cli", "hosted"} {
		tests := []struct {
			name, query, index string
			args               []any
		}{
			{"dirty", `SELECT EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='dirty')`, "review_comparison_proofs_state", []any{attemptProject, current.Pull.Number, channel}},
			{"pending", `SELECT EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='pending')`, "review_comparison_proofs_state", []any{attemptProject, current.Pull.Number, channel}},
			{"ready", `SELECT EXISTS(SELECT 1 FROM review_comparison_proofs WHERE project_id=? AND pull_number=? AND channel=? AND state='ready' AND repository_id=? AND candidate_sha=? AND base_sha=? AND parent_sha=? AND parent_pull_number=? AND base_ref=?)`, "review_comparison_proofs_anchor", []any{attemptProject, current.Pull.Number, channel, current.Repository.ID, current.Pull.HeadSHA, current.Pull.BaseSHA, current.ParentSHA, current.ParentPullNumber, current.Pull.BaseRef}},
		}
		for _, test := range tests {
			rows, err := f.store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+test.query, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				plan.WriteString(detail)
				plan.WriteByte('\n')
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			text := plan.String()
			if !strings.Contains(text, "SEARCH review_comparison_proofs USING COVERING INDEX "+test.index) || strings.Contains(text, "SCAN review_") || strings.Contains(text, "TEMP B-TREE") || strings.Contains(text, "review_execution_evidence") {
				t.Fatalf("comparison %s/%s lost indexed derived-only lookup:\n%s", channel, test.name, text)
			}
		}
	}
}

func TestComparisonGrowthClosedOlderCapturesDoNotHoldCurrentReady(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	source, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 102; i++ {
		next := coverageNextAttempt(t, f, fmt.Sprintf("closed-growth-%d", i), "cli", strings.Repeat("9", 40))
		capture := source.Capture
		capture.ExpectedAnchor = *next.Anchor
		// Distinct synthetic captures enter only through the trusted ingestion API.
		capture.CaptureSHA256 = fmt.Sprintf("%032x%032x", i+1, i+1)
		capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
		recorded, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
		if err != nil {
			t.Fatal(err)
		}
		adjudicationComplete(t, f, next)
		body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), fmt.Sprintf("closed-finalize-%d", i), recorded.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
		adjudicationReceipt(t, body, err, recorded.ID, "finalized", 1)
	}
	coverageComparison(t, f, a, current, ComparisonState{Covered: true})
	var ready int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_comparison_proofs WHERE project_id=? AND pull_number=2 AND channel='cli' AND state='ready'`, attemptProject).Scan(&ready); err != nil || ready != 103 {
		t.Fatal("closed growth did not retain all native proofs", ready, err)
	}
	if err := ValidateComparisonProofs(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
