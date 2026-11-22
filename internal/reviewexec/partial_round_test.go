package reviewexec

import (
	"fmt"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestUsablePartialRoundAdjudicationAndTaper(t *testing.T) {
	for _, test := range []struct {
		name, reason, disposition string
		wantUsed, wantDry         int64
	}{
		{"empty settled subset", CLIUsablePartialReason, "", 1, 1},
		{"all findings rejected", CLIUsablePartialReason, "rejected", 1, 1},
		{"accepted finding", CLIUsablePartialReason, "accepted", 1, 0},
		{"historic unqualified capture", "coverage_incomplete", "rejected", 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := roundFixture(t)
			roundGrant(t, f, 2, "cli", 0, 0, 3, "partial-round-grant")
			receipt := f.enqueue(t, attemptProject, "partial-round-op", "cli", 2, 1)
			roundBind(t, f, receipt.ID)
			attempt := f.start(t, attemptProject, receipt)
			anchor := attemptAnchor(attemptProject, 2)
			anchor.ParentSHA = anchor.BaseSHA
			var err error
			attempt, err = f.e.Admit(t.Context(), attemptProject, attempt.ID, attempt.Revision, anchor, "")
			if err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("%064x", 2)
			input := ReviewEvidenceInput{
				CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024,
				StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
				ExpectedAnchor: anchor,
				Review: reviewprovider.ParsedReview{
					Outcome: "failed", ReviewedFiles: []string{"checked.go"}, Findings: []reviewprovider.ParsedFinding{},
				},
			}
			if test.disposition != "" {
				input.Review.ReportedFindings = 1
				input.Review.Findings = append(input.Review.Findings, reviewprovider.ParsedFinding{
					Title: "Retained finding", Detail: "Reviewed path", Severity: "high", Path: "checked.go",
				})
			}
			evidence, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, attempt.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.e.Advance(t.Context(), attemptProject, attempt.ID, attempt.Revision, "failed", test.reason, ""); err != nil {
				t.Fatal(err)
			}
			if test.disposition != "" {
				if progress := roundRead(t, f, 2, "cli"); progress.Used != 0 {
					t.Fatalf("partial with undecided findings spent a round: %+v", progress)
				}
				view, err := f.e.readRoundProgress(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), []int64{2}, roundProgressView{stages: true})
				if err != nil {
					t.Fatal(err)
				}
				cli := view.Items[0].Channels[1]
				if cli.PendingAdjudication == nil || *cli.PendingAdjudication != test.wantUsed || cli.PendingExecution == nil || *cli.PendingExecution != 0 {
					t.Fatalf("unadjudicated slot does not distinguish qualified evidence: %+v", cli)
				}
				adjudicationDecide(t, f, evidence.ID, "settle-subset", 0, 0, test.disposition)
			}
			progress := roundRead(t, f, 2, "cli")
			if progress.Used != test.wantUsed || progress.Dry != test.wantDry {
				t.Fatalf("round count = %d/%d, want %d/%d", progress.Used, progress.Dry, test.wantUsed, test.wantDry)
			}
			wantAccepted := int64(0)
			if test.disposition == "accepted" {
				wantAccepted = 1
			}
			if test.wantUsed == 0 {
				assertAutoFinalizationAbsent(t, f, evidence.ID, 1)
			} else {
				assertAutoFinalizedAndPublished(t, f, evidence.ID, 1, FindingCounts{Found: input.Review.ReportedFindings, Accepted: wantAccepted})
				var partial bool
				err = f.store.DB().QueryRow("SELECT json_extract(body_json,'$.payload.partial') FROM review_checkpoint_publications WHERE evidence_id=?", evidence.ID).Scan(&partial)
				if err != nil || !partial {
					t.Fatalf("checkpoint did not identify partial coverage: %v", err)
				}
			}
			if comparisonRows := attemptCount(t, f.store, "SELECT count(*) FROM review_comparison_proofs WHERE operation_id=?", receipt.ID); comparisonRows != 0 {
				t.Fatalf("partial acquired a full comparison proof")
			}
			if wantAccepted == 1 {
				plan := SourcePlan{
					Repository: reviewprovider.Repository{ID: anchor.RepositoryID, FullName: anchor.Repository, DefaultBranch: anchor.BaseRef},
					Pull: reviewprovider.Pull{Number: anchor.PullNumber, RepositoryID: anchor.RepositoryID, State: "open",
						HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: anchor.BaseRef, ChangedFiles: 2},
					ParentSHA: anchor.ParentSHA, ObservedAt: anchor.ObservedAt,
				}
				state, err := f.e.Comparison(t.Context(), attemptProject, plan, "cli")
				if err != nil || !state.Pending || state.Covered {
					t.Fatalf("unfixed partial finding did not retain its hold: %+v %v", state, err)
				}
				_, fixes, err := retainedCLIObligations(t.Context(), f.store.DB(), attemptProject, anchor.PullNumber)
				if err != nil || !fixes {
					t.Fatalf("unfixed partial finding was absent from the fix summary: %v", err)
				}
				body, err := f.e.RecordOperationalFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "repair-subset-finding", evidence.ID,
					OperationalFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedDigest: adjudicationSHA(""), ProofNote: "Verified the reported operational fix."})
				adjudicationReceipt(t, body, err, evidence.ID, "operational_fix_recorded", 2)
				state, err = f.e.Comparison(t.Context(), attemptProject, plan, "cli")
				if err != nil || state.Pending || state.Covered {
					t.Fatalf("operational proof must clear the hold without claiming coverage: %+v %v", state, err)
				}
				_, fixes, err = retainedCLIObligations(t.Context(), f.store.DB(), attemptProject, anchor.PullNumber)
				if err != nil || fixes {
					t.Fatalf("settled partial finding remained in the fix summary: %v", err)
				}
			}
			before := roundTables(t, f)
			roundCompleteHook(t, f, receipt.ID)
			if before != roundTables(t, f) {
				t.Fatal("repeat credit spent a second review round")
			}
			if err = ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
				t.Fatalf("stored round snapshot does not replay: %v", err)
			}
		})
	}
}
