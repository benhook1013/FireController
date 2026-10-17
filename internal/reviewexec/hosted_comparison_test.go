package reviewexec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedComparisonFixture(t *testing.T, zero bool) (*attemptFixture, Attempt, string, SourcePlan) {
	t.Helper()
	f, a, in := hostedPublicationFixture(t, zero)
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	anchor := *a.Anchor
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: anchor.RepositoryID, FullName: anchor.Repository, DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: anchor.PullNumber, RepositoryID: anchor.RepositoryID, State: "open", HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: anchor.BaseRef, ChangedFiles: 1}, ParentSHA: anchor.ParentSHA, ObservedAt: stamp()}
	return f, a, receipt.ID, plan
}

func hostedComparisonRead(t *testing.T, f *attemptFixture, a Attempt, plan SourcePlan, want ComparisonState) {
	t.Helper()
	before := adjudicationState(t, f, a)
	got, err := f.e.Comparison(t.Context(), attemptProject, plan, "hosted")
	if err != nil || got != want {
		t.Fatalf("hosted comparison got %+v want %+v: %v", got, want, err)
	}
	usable, err := f.e.Coverage(t.Context(), attemptProject, plan, "hosted")
	if err != nil || usable != (want.Covered && !want.Pending) {
		t.Fatal("hosted effective coverage", usable, err)
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("comparison changed retained state")
	}
}

func TestHostedComparisonZeroRequiresCompletedAttemptAndFinalization(t *testing.T) {
	f, a, id, current := hostedComparisonFixture(t, true)
	hostedComparisonRead(t, f, a, current, ComparisonState{}) // active is not completed evidence
	adjudicationComplete(t, f, a)
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-zero-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, body, err, id, "finalized", 1)
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true})
	// Native hosted credit and pending obligations must never become CLI credit.
	coverageComparison(t, f, a, current, ComparisonState{})
}

func TestHostedComparisonFrozenCountsCorrectionsAndMovedHead(t *testing.T) {
	f, a, id, current := hostedComparisonFixture(t, false)
	adjudicationComplete(t, f, a)
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	adjudicationDecide(t, f, id, "hosted-accept", 0, 0, "accepted")
	adjudicationDecide(t, f, id, "hosted-reject-one", 1, 1, "rejected")
	adjudicationDecide(t, f, id, "hosted-reject-two", 2, 2, "rejected")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(3)})
	adjudicationReceipt(t, body, err, id, "finalized", 4)
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	moved := current
	moved.Pull.HeadSHA = strings.Repeat("9", 40)
	hostedComparisonRead(t, f, a, moved, ComparisonState{Pending: true})
	coverageFix(t, f, id, "hosted-fix-one", 4, 0, "")
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true})
	hostedComparisonRead(t, f, a, moved, ComparisonState{})
	adjudicationDecide(t, f, id, "hosted-correct", 5, 1, "accepted")
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	hostedComparisonRead(t, f, a, moved, ComparisonState{Pending: true})
	coverageFix(t, f, id, "hosted-fix-two", 6, 1, "")
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true})
	state := adjudicationRead(t, f, id)
	if state.FrozenCounts == nil || *state.FrozenCounts != (FindingCounts{Found: 3, Accepted: 1}) {
		t.Fatal("current correction rewrote frozen counts", state)
	}
	coverageComparison(t, f, a, current, ComparisonState{})
}

func TestHostedComparisonExactScopeAndRepositoryBinding(t *testing.T) {
	f, a, id, current := hostedComparisonFixture(t, true)
	coverageFinalize(t, f, a, id, 0)
	cases := map[string]func(*SourcePlan){
		"head":             func(p *SourcePlan) { p.Pull.HeadSHA = strings.Repeat("9", 40) },
		"effective parent": func(p *SourcePlan) { p.ParentSHA = strings.Repeat("8", 40); p.Pull.BaseSHA = p.ParentSHA },
		"parent PR":        func(p *SourcePlan) { p.ParentPullNumber = 1 },
		"base ref":         func(p *SourcePlan) { p.ParentPullNumber = 1; p.Pull.BaseRef = "feature-parent" },
		"pull":             func(p *SourcePlan) { p.Pull.Number = 3 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			plan := current
			change(&plan)
			hostedComparisonRead(t, f, a, plan, ComparisonState{})
		})
	}
	renamed := current
	renamed.Repository.FullName = "renamed/repository"
	hostedComparisonRead(t, f, a, renamed, ComparisonState{Covered: true})
	foreign := current
	foreign.Repository.ID++
	foreign.Pull.RepositoryID++
	_, err := f.e.Comparison(t.Context(), attemptProject, foreign, "hosted")
	attemptCode(t, err, "review_held")
}

func TestHostedComparisonReadyCannotMaskMissingCaptureAcrossHeads(t *testing.T) {
	f, a, id, current := hostedComparisonFixture(t, true)
	coverageFinalize(t, f, a, id, 0)
	comparisonCompleteMissingCapture(t, f, "hosted-older-missing", "hosted", strings.Repeat("9", 40))
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true, Pending: true})
	coverageComparison(t, f, a, current, ComparisonState{})
}

func TestComparisonChannelsRemainIndependent(t *testing.T) {
	f, a, id, current := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	comparisonCompleteMissingCapture(t, f, "independent-hosted-missing", "hosted", current.Pull.HeadSHA)
	coverageComparison(t, f, a, current, ComparisonState{Covered: true})
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	// Reject channel errors before touching even a nil engine/store.
	var absent *Engine
	for _, channel := range []string{"", "CLI", "Hosted", "manual", "cli ", "hosted\x00"} {
		_, err := absent.Comparison(t.Context(), attemptProject, current, channel)
		attemptCode(t, err, "validation_failed")
		_, err = absent.Coverage(t.Context(), attemptProject, current, channel)
		attemptCode(t, err, "validation_failed")
	}
}

func TestHostedComparisonRejectsCorruptRetainedIdentity(t *testing.T) {
	cases := map[string]func(*testing.T, *attemptFixture, Attempt, string){
		"trigger": func(t *testing.T, f *attemptFixture, a Attempt, _ string) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET external_id='999' WHERE id=?`, a.ID)
		},
		"intent": func(t *testing.T, f *attemptFixture, a Attempt, _ string) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET intent='different intent' WHERE id=?`, a.ID)
		},
		"attempt state": func(t *testing.T, f *attemptFixture, a Attempt, _ string) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET state='active' WHERE id=?`, a.ID)
		},
		"metadata": func(t *testing.T, f *attemptFixture, _ Attempt, id string) {
			var raw string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var capture ReviewEvidence
			if err := json.Unmarshal([]byte(raw), &capture); err != nil {
				t.Fatal(err)
			}
			capture.Capture.Hosted.ObservedBaseSHA = strings.Repeat("9", 40)
			body, err := json.Marshal(capture)
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=? WHERE id=?`, string(body), id)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			f, a, id, current := hostedComparisonFixture(t, true)
			coverageFinalize(t, f, a, id, 0)
			damage(t, f, a, id)
			before := adjudicationState(t, f, a)
			got, err := f.e.Comparison(t.Context(), attemptProject, current, "hosted")
			attemptCode(t, err, "storage_unavailable")
			if got != (ComparisonState{}) || adjudicationState(t, f, a) != before {
				t.Fatal("malformed evidence gave credit or comparison changed state")
			}
		})
	}
}

func TestHostedComparisonClosedRunCannotMaskUnfinalizedRun(t *testing.T) {
	f, a, id, current := hostedComparisonFixture(t, true)
	coverageFinalize(t, f, a, id, 0)
	source, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	next := f.start(t, attemptProject, f.enqueue(t, attemptProject, "second-hosted-intent", "hosted", 2, 1))
	anchor := *a.Anchor
	anchor.ObservedAt = stamp()
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: next.OperationID, AttemptID: next.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	next, err = f.e.Admit(t.Context(), attemptProject, next.ID, next.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	next, err = f.e.Advance(t.Context(), attemptProject, next.ID, next.Revision, "active", "posted", "101")
	if err != nil {
		t.Fatal(err)
	}
	capture := source.Capture
	capture.ExpectedAnchor = anchor
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, next.ID, capture)
	if err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, next)
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true, Pending: true})
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "second-hosted-finalize", receipt.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, body, err, receipt.ID, "finalized", 1)
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true})
}

func TestHostedComparisonCompletedMissingCaptureGrowth(t *testing.T) {
	f, a, _, current := hostedComparisonFixture(t, true)
	adjudicationComplete(t, f, a)
	comparisonMissingHistory(t, f, 102, "hosted", strings.Repeat("9", 40))
	hostedComparisonRead(t, f, a, current, ComparisonState{Pending: true})
	coverageComparison(t, f, a, current, ComparisonState{})
}

func TestComparisonVersionFourUpgradePreservesJournal(t *testing.T) {
	f, a, id, current := hostedComparisonFixture(t, true)
	coverageFinalize(t, f, a, id, 0)
	before := adjudicationState(t, f, a)
	admissionSQL(t, f, `DROP TABLE review_hosted_preflight`)
	admissionSQL(t, f, `DROP TABLE review_hosted_allowance_queries`)
	admissionSQL(t, f, `DROP INDEX review_operations_completed_comparison`)
	admissionSQL(t, f, `CREATE INDEX review_operations_cli_admission ON review_operations(project_id,pull_number,channel,state,id)`)
	admissionSQL(t, f, `UPDATE metadata SET value='4' WHERE key='schema:review_execution'`)
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != "6" {
		t.Fatal("ordered comparison migration", version, err)
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("index migration changed retained journal/evidence")
	}
	hostedComparisonRead(t, f, a, current, ComparisonState{Covered: true})
}
