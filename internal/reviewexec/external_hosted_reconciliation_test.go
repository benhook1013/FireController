package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func reconciliationFixture(t *testing.T, kind string) (*attemptFixture, Attempt, ReviewEvidenceInput, reviewprovider.ExternalHostedRun, lifecycleTarget) {
	t.Helper()
	f, a, in := hostedPublicationFixture(t, false)
	if kind == "standing" {
		f, a, in = standingEvidenceFixture(t)
	}
	if kind == "submitted" {
		in.Hosted.PublicationID, in.Hosted.ProviderAttemptID = "", ""
		in.Hosted.ReviewIDs = []int64{201}
		in.Hosted.Sources = in.Hosted.Sources[:1]
		in.Review.Findings, in.Review.ReportedFindings = in.Review.Findings[:1], 1
		in.Hosted.SubmittedReview = &HostedSubmittedReviewMetadata{ProviderRunID: "33333333-3333-3333-3333-333333333333", AcknowledgementCommentID: 102}
	}
	// Actual predecessor ordering: visible metadata/frontier existed before either
	// producer observed this provider result.
	response := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "seed-reconciliation", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: a.Anchor.RepositoryID, Source: "synthetic-metadata", ObservedAt: stamp(), Queue: []reviews.PullInput{projectionPull(2)}}))
	if response.Code != 201 {
		t.Fatalf("metadata fixture %d %s", response.Code, response.Body.String())
	}
	target := lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := reviews.InitializeExternalHostedFrontier(ctx, tx, target.project, target.repository, target.number)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, in.FinishedAt)
	if err != nil {
		t.Fatal(err)
	}
	run := reviewprovider.ExternalHostedRun{SourceID: nativeHostedExternalSourceID(in.Hosted), HeadSHA: a.Anchor.CandidateSHA, ReviewIDs: append([]int64{}, in.Hosted.ReviewIDs...), FinishedAt: at, Inventory: reviewprovider.HostedFindingInventory{Complete: true, Publication: reviewprovider.HostedPublication{PublicationID: in.Hosted.PublicationID, ProviderAttemptID: in.Hosted.ProviderAttemptID}, Findings: []reviewprovider.HostedFinding{}}}
	if in.Hosted.SubmittedReview != nil {
		run.ProviderRunID = in.Hosted.SubmittedReview.ProviderRunID
	}
	if in.Hosted.StandingSummary != nil {
		run.ProviderRunID = in.Hosted.StandingSummary.ProviderRunID
	}
	for i, finding := range in.Review.Findings {
		run.Inventory.Findings = append(run.Inventory.Findings, reviewprovider.HostedFinding{SourceKey: in.Hosted.Sources[i].SourceKey, Title: finding.Title, Detail: finding.Detail, Severity: finding.Severity})
	}
	return f, a, in, run, target
}

func reconciliationObserve(t *testing.T, f *attemptFixture, target lifecycleTarget, run reviewprovider.ExternalHostedRun, limit int) (int, bool) {
	t.Helper()
	count, held := 0, false
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		inputs, h, err := f.e.externalHostedInputs(ctx, tx, target, []reviewprovider.ExternalHostedRun{run}, limit)
		if err != nil {
			return err
		}
		count, held = len(inputs), h
		other, err := publishExternalHostedInputs(ctx, tx, target, inputs)
		held = held || other
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return count, held
}
func reconciliationCurrent(t *testing.T, f *attemptFixture) reviews.PullResponse {
	t.Helper()
	var p reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &p)
	return p
}

func TestHostedExactAliasReconcilesBothPublicationOrders(t *testing.T) {
	for _, kind := range []string{"publication", "submitted", "standing"} {
		for _, order := range []string{"observer_first", "native_first", "legacy_native_replay", "legacy_observer_tick"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				f, a, in, external, target := reconciliationFixture(t, kind)
				var oldSnapshot, oldDigest string
				if order != "native_first" {
					count, held := reconciliationObserve(t, f, target, external, 1)
					if held || count != 1 {
						t.Fatal("observer-first fixture", count, held)
					}
					oldSnapshot = reconciliationCurrent(t, f).SnapshotID
					oldDigest = projectionDigest(t, f, oldSnapshot)
				}
				captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
				if err != nil {
					t.Fatal(err)
				}
				evidence, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), captured.ID)
				if err != nil {
					t.Fatal(err)
				}
				pull := projectionPull(2)
				pull.HeadSHA = strings.Repeat("f", 40)
				pull.Title = "Current metadata after captured head moved"
				if strings.HasPrefix(order, "legacy_") {
					run, err := publicationRun(evidence, adjudicationRead(t, f, captured.ID), a)
					if err != nil {
						t.Fatal(err)
					}
					// The pre-fix publisher had no external alias. Preserve that exact existing
					// state so ordinary replay/tick must repair it without a manual command.
					projectionPublish(t, f, reviews.NativePublication{Pull: pull, Run: &run})
					if reconciliationCurrent(t, f).Pull.RunCounts.Hosted != 2 {
						t.Fatal("legacy duplicate fixture")
					}
				} else {
					if _, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, &pull); err != nil {
						t.Fatal(err)
					}
				}
				beforeState := adjudicationRead(t, f, captured.ID)
				beforeAttempt := attemptState(t, f, a)
				beforeCredit := attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`)
				var receipt reviews.Receipt
				if order == "legacy_observer_tick" || order == "native_first" {
					count, held := reconciliationObserve(t, f, target, external, 1)
					if count != 0 || held {
						t.Fatal("native source reimported or held", count, held)
					}
				} else {
					receipt, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				current := reconciliationCurrent(t, f)
				if current.Pull.RunCounts.Hosted != 1 || current.Pull.Title != pull.Title || current.Pull.HeadSHA != pull.HeadSHA {
					t.Fatal("counts or newer metadata lost", current.Pull.RunCounts, current.Pull.Title, current.Pull.HeadSHA)
				}
				if receipt.ID != "" && receipt.ID != current.SnapshotID {
					t.Fatal("replay returned obsolete receipt")
				}
				detail := cliPublicationDetail(t, f, 2, captured.ID)
				if detail.Run.SourceRunID != captured.ID || detail.Run.HeadSHA != in.ExpectedAnchor.CandidateSHA || detail.Run.Found != in.Review.ReportedFindings || len(detail.Findings) != len(in.Review.Findings) {
					t.Fatal("native identity/findings changed")
				}
				for i, finding := range detail.Findings {
					if finding.Title != in.Review.Findings[i].Title || finding.Detail != in.Review.Findings[i].Detail || finding.SourceFindingID != projectionFindingID(captured.ID, int64(i)) {
						t.Fatal("native original finding replaced")
					}
				}
				if !reflect.DeepEqual(beforeState, adjudicationRead(t, f, captured.ID)) || beforeAttempt != attemptState(t, f, a) || beforeCredit != attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) {
					t.Fatal("reconciliation altered native adjudication/execution/credit")
				}
				if oldSnapshot != "" && projectionDigest(t, f, oldSnapshot) != oldDigest {
					t.Fatal("observer historical snapshot changed")
				}
				snapshots := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
				if _, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, nil); err != nil {
					t.Fatal(err)
				}
				if count, held := reconciliationObserve(t, f, target, external, 1); count != 0 || held {
					t.Fatal("reconciled replay changed", count, held)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != snapshots {
					t.Fatal("exact replay appended another projection")
				}
			})
		}
	}
}

func TestHostedRetainedObserverRequiresCurrentExactNativeReplacement(t *testing.T) {
	for _, mode := range []string{"capture_only", "active_only", "different_head", "different_reviews", "different_attempt", "different_source", "conflicting_retained_id"} {
		t.Run(mode, func(t *testing.T) {
			f, a, in, external, target := reconciliationFixture(t, "publication")
			if count, held := reconciliationObserve(t, f, target, external, 1); count != 1 || held {
				t.Fatal("seed", count, held)
			}
			if mode != "active_only" {
				captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "capture_only" {
					e, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), captured.ID)
					if err != nil {
						t.Fatal(err)
					}
					run, err := publicationRun(e, adjudicationRead(t, f, captured.ID), a)
					if err != nil {
						t.Fatal(err)
					}
					projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
				}
			}
			switch mode {
			case "different_head":
				external.HeadSHA = strings.Repeat("e", 40)
			case "different_reviews":
				external.ReviewIDs = []int64{999}
			case "different_attempt":
				external.Inventory.Publication.ProviderAttemptID = "99999999-9999-4999-8999-999999999999"
			case "different_source":
				external.SourceID = "publication:99999999-9999-4999-8999-999999999999"
			case "conflicting_retained_id":
				admissionSQL(t, f, `UPDATE review_run_versions SET head_sha=? WHERE source='github-hosted-observer'`, strings.Repeat("e", 40))
			}
			before := reconciliationCurrent(t, f)
			count, held := reconciliationObserve(t, f, target, external, 1)
			if count != 0 {
				t.Fatal("unproven alias appended", count)
			}
			if mode != "capture_only" && mode != "active_only" && !held {
				t.Fatal("mismatched source not held")
			}
			if after := reconciliationCurrent(t, f); after.SnapshotID != before.SnapshotID || after.Pull.RunCounts != before.Pull.RunCounts {
				t.Fatal("observer hidden without exact current native")
			}
		})
	}
}

func TestHostedKnownNativeAliasesDoNotSpendAppendBudget(t *testing.T) {
	f, a, in, external, target := reconciliationFixture(t, "publication")
	if count, held := reconciliationObserve(t, f, target, external, 1); count != 1 || held {
		t.Fatal(count, held)
	}
	capture, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, capture.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	next := external
	next.SourceID = "publication:99999999-9999-4999-8999-999999999999"
	next.Inventory.Publication.PublicationID = "99999999-9999-4999-8999-999999999999"
	next.ReviewIDs = []int64{999}
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		inputs, held, err := f.e.externalHostedInputs(ctx, tx, target, []reviewprovider.ExternalHostedRun{external, external, external, next}, 1)
		if err != nil {
			return err
		}
		if held || len(inputs) != 1 || inputs[0].SourceRunID != next.SourceID {
			return fmt.Errorf("known native aliases starved append budget: %d/%v", len(inputs), held)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIHasNoHostedExternalAlias(t *testing.T) {
	if nativeHostedExternalSourceID(nil) != "" {
		t.Fatal("CLI assigned hosted alias")
	}
}

func TestHostedNativeReplayRetainsExistingAdjudication(t *testing.T) {
	f, a, in, external, target := reconciliationFixture(t, "publication")
	captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	adjudicationDecide(t, f, captured.ID, "accepted-before-duplicate", 0, 0, "accepted")
	before := cliPublicationDetail(t, f, 2, captured.ID)
	ledger := adjudicationRead(t, f, captured.ID)
	if before.Run.Accepted != 1 {
		t.Fatal("real native decision missing")
	}
	// The pre-fix passive publisher could append after native capture. Exercise
	// that predecessor's raw publication path, then repair through ordinary replay.
	var observer reviews.RunInput
	observer = reviews.RunInput{SourceRunID: external.SourceID, Channel: "hosted", HeadSHA: external.HeadSHA, Reviewer: "CodeRabbit", Scope: "external hosted publication", Outcome: "completed", Complete: true, Attributable: true, StartedAt: in.FinishedAt, FinishedAt: in.FinishedAt, Found: in.Review.ReportedFindings, Findings: []reviews.Finding{}, CoverageLimits: []string{}}
	for _, finding := range external.Inventory.Findings {
		observer.Findings = append(observer.Findings, reviews.Finding{SourceFindingID: finding.SourceKey, Title: finding.Title, Detail: finding.Detail, Severity: finding.Severity, Disposition: "unadjudicated"})
	}
	if err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		held, err := publishExternalHostedInputs(ctx, tx, target, []reviews.RunInput{observer})
		if held {
			return fmt.Errorf("predecessor duplicate rejected")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if reconciliationCurrent(t, f).Pull.RunCounts.Hosted != 2 {
		t.Fatal("not a duplicate fixture")
	}
	old := reconciliationCurrent(t, f)
	digest := projectionDigest(t, f, old.SnapshotID)
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, nil); err != nil {
		t.Fatal(err)
	}
	after := cliPublicationDetail(t, f, 2, captured.ID)
	if !reflect.DeepEqual(before.Run, after.Run) || !reflect.DeepEqual(before.Findings, after.Findings) || !reflect.DeepEqual(ledger, adjudicationRead(t, f, captured.ID)) || reconciliationCurrent(t, f).Pull.RunCounts.Hosted != 1 {
		t.Fatal("native adjudication or identity changed")
	}
	if projectionDigest(t, f, old.SnapshotID) != digest {
		t.Fatal("old duplicate history changed")
	}
}
