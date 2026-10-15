package reviewruntime

import (
	"context"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// CapturedHosted reports retained source facts only. Complete means a supported
// final capture was recorded, not current coverage, readiness or settlement.
type CapturedHosted struct {
	Capture  CaptureReference   `json:"capture"`
	Receipt  reviewexec.Receipt `json:"receipt"`
	Complete bool               `json:"complete"`
	Reason   string             `json:"reason"`
}

// RetainHosted saves a private observation before deriving final evidence. A
// pending or unsupported observation leaves the attempt's final evidence slot
// free for a later complete capture. It performs no provider work, publication
// or attempt transition; the journal rechecks the actual retained binding.
func RetainHosted(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, record HostedCaptureRecord) (CapturedHosted, error) {
	if ctx == nil || captures == nil || engine == nil || attempt.Anchor == nil || !validIdentifier(attempt.ProjectID) {
		return CapturedHosted{}, core.Fail("validation_failed", "Use the admitted hosted attempt and its scoped capture.", 400)
	}
	anchor := *attempt.Anchor
	intent := reviewprovider.HostedTriggerIntent{OperationID: attempt.OperationID, AttemptID: attempt.ID, HeadSHA: anchor.CandidateSHA}
	if _, err := reviewprovider.HostedTriggerBody(intent); err != nil {
		return CapturedHosted{}, core.Fail("validation_failed", "Use the admitted hosted attempt and its scoped capture.", 400)
	}
	ref, err := captures.Save(ctx, attempt.ID, record)
	if err != nil {
		return CapturedHosted{}, err
	}
	result := CapturedHosted{Capture: ref, Reason: "capture_retained"}
	retained, err := captures.Read(ctx, attempt.ID, ref)
	if err != nil {
		return result, err
	}
	if retained.RepositoryID != anchor.RepositoryID || retained.Repository != anchor.Repository || retained.PullNumber != anchor.PullNumber || retained.Intent != intent {
		result.Reason = "source_changed"
		return result, nil
	}
	command, _ := reviewprovider.HostedTriggerBody(intent)
	_, err = engine.RecordHostedObservation(ctx, attempt.ProjectID, attempt.ID, reviewexec.HostedObservationInput{
		CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes,
		StartedAt: retained.StartedAt.Format(time.RFC3339Nano), FinishedAt: retained.FinishedAt.Format(time.RFC3339Nano), ObservedAt: retained.Snapshot.ObservedAt.Format(time.RFC3339Nano),
		ExpectedAnchor: anchor, Intent: command,
	})
	if err != nil {
		result.Reason = "observation_unavailable"
		return result, err
	}
	inventory, err := reviewprovider.ExtractHostedFindings(retained.Snapshot, retained.Intent, retained.PostingActor, retained.ProviderActor, retained.EarliestPosting)
	if err != nil {
		result.Reason = "capture_invalid"
		return result, nil
	}
	if !inventory.Complete || inventory.Publication.State != "attributed" {
		result.Reason = inventory.Reason
		if result.Reason == "" {
			result.Reason = "finding_inventory_incomplete"
		}
		return result, nil
	}
	publication := inventory.Publication
	base, proven := reviewprovider.ResolveHostedComparison(publication, anchor.CandidateSHA, anchor.ParentSHA, anchor.MergeBaseSHA)
	if !proven {
		result.Reason = "comparison_unproven"
		return result, nil
	}
	review := reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: int64(len(inventory.Findings)), ReviewedFiles: []string{}, Findings: make([]reviewprovider.ParsedFinding, 0, len(inventory.Findings))}
	metadata := &reviewexec.HostedEvidenceMetadata{PublicationID: publication.PublicationID, ProviderAttemptID: publication.ProviderAttemptID, TriggerCommentID: publication.Trigger.CommentID, ReviewIDs: make([]int64, 0, len(publication.Batches)), ObservedBaseSHA: base, Sources: make([]reviewexec.HostedFindingSource, 0, len(inventory.Findings))}
	for _, batch := range publication.Batches {
		metadata.ReviewIDs = append(metadata.ReviewIDs, batch.ID)
	}
	for _, finding := range inventory.Findings {
		plain := reviewprovider.ParsedFinding{Title: finding.Title, Detail: finding.Detail, Severity: finding.Severity, Path: finding.Path}
		if finding.StartLine != nil {
			plain.StartLine = *finding.StartLine
		}
		if finding.EndLine != nil {
			plain.EndLine = *finding.EndLine
		}
		review.Findings = append(review.Findings, plain)
		metadata.Sources = append(metadata.Sources, reviewexec.HostedFindingSource{SourceKey: finding.SourceKey, Fingerprint: finding.Fingerprint, Kind: finding.Kind, Side: finding.Side, ReviewID: finding.ReviewID, CommentID: finding.CommentID})
	}
	input := reviewexec.ReviewEvidenceInput{CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: publication.Trigger.CreatedAt.Format(time.RFC3339Nano), FinishedAt: publication.FinishedAt.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: review, Hosted: metadata}
	result.Receipt, err = engine.RecordReviewEvidence(ctx, attempt.ProjectID, attempt.ID, input)
	if err != nil {
		result.Reason = "evidence_unavailable"
		return result, err
	}
	result.Complete, result.Reason = true, "capture_recorded"
	return result, nil
}
