package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
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
	result, retained, err := retainHostedObservation(ctx, captures, engine, attempt, record)
	if err != nil || result.Reason != "capture_retained" {
		return result, err
	}
	anchor := *attempt.Anchor
	ref := result.Capture
	inventory, err := reviewprovider.ExtractHostedFindings(retained.Snapshot, retained.Intent, retained.PostingActor, retained.ProviderActor, retained.EarliestPosting)
	if err != nil {
		result.Reason = "capture_invalid"
		return result, nil
	}
	var baselineRef CaptureReference
	if inventory.Reason == "standing_zero_baseline_missing" {
		baseline, ref, baselineErr := hostedStandingBaseline(ctx, captures, engine, attempt, retained, result.Capture)
		if baselineErr != nil {
			result.Reason = "evidence_unavailable"
			return result, baselineErr
		}
		baselineRef = ref
		inventory, err = reviewprovider.ExtractHostedFindingsWithBaseline(retained.Snapshot, retained.Intent, retained.PostingActor, retained.ProviderActor, retained.EarliestPosting, baseline)
		if err != nil {
			result.Reason = "capture_invalid"
			return result, nil
		}
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
	if zero := publication.StandingZero; zero != nil {
		metadata.StandingZero = &reviewexec.HostedStandingZeroMetadata{CommentID: zero.CommentID, CompletionCommentID: zero.CompletionCommentID, RunID: zero.RunID, BaselineCaptureID: baselineRef.ID, BaselineCaptureSHA256: baselineRef.SHA256}
	}
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
	canonical, err := json.Marshal(input)
	if err != nil {
		result.Reason = "evidence_unavailable"
		return result, core.Fail("validation_failed", "The parsed review capture could not be encoded.", 400)
	}
	// Source-level finding bounds do not bound canonical JSON: HTML characters
	// expand when encoded. Keep the raw journaled observation inspectable while
	// leaving the unique final-evidence slot free if the ledger cannot hold it.
	if len(canonical) > reviewexec.MaxEvidenceInputBytes {
		result.Reason = "finding_inventory_limit"
		return result, nil
	}
	result.Receipt, err = engine.RecordReviewEvidence(ctx, attempt.ProjectID, attempt.ID, input)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "validation_failed" {
			// Provider-shaped source can still fail the stricter ledger contract.
			// Keep its raw observation inspectable without occupying the final slot.
			result.Receipt = reviewexec.Receipt{}
			result.Reason = "capture_invalid"
			return result, nil
		}
		result.Reason = "evidence_unavailable"
		return result, err
	}
	result.Complete, result.Reason = true, "capture_recorded"
	return result, nil
}

// retainHostedObservation journals immutable raw source before trigger recovery
// or final interpretation. It never changes the attempt or evidence slot.
func retainHostedObservation(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, record HostedCaptureRecord) (CapturedHosted, HostedCaptureRecord, error) {
	if ctx == nil || captures == nil || engine == nil || attempt.Anchor == nil || !validIdentifier(attempt.ProjectID) {
		return CapturedHosted{}, HostedCaptureRecord{}, core.Fail("validation_failed", "Use the admitted hosted attempt and its scoped capture.", 400)
	}
	anchor := *attempt.Anchor
	intent := reviewprovider.HostedTriggerIntent{OperationID: attempt.OperationID, AttemptID: attempt.ID, HeadSHA: anchor.CandidateSHA}
	if _, err := reviewprovider.HostedTriggerBody(intent); err != nil {
		return CapturedHosted{}, HostedCaptureRecord{}, core.Fail("validation_failed", "Use the admitted hosted attempt and its scoped capture.", 400)
	}
	ref, err := captures.Save(ctx, attempt.ID, record)
	if err != nil {
		return CapturedHosted{}, HostedCaptureRecord{}, err
	}
	result := CapturedHosted{Capture: ref, Reason: "capture_retained"}
	retained, err := captures.Read(ctx, attempt.ID, ref)
	if err != nil {
		return result, HostedCaptureRecord{}, err
	}
	if retained.RepositoryID != anchor.RepositoryID || retained.Repository != anchor.Repository || retained.PullNumber != anchor.PullNumber || retained.Intent != intent {
		result.Reason = "source_changed"
		return result, retained, nil
	}
	command, _ := reviewprovider.HostedTriggerBody(intent)
	_, err = engine.RecordHostedObservation(ctx, attempt.ProjectID, attempt.ID, reviewexec.HostedObservationInput{
		CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes,
		StartedAt: retained.StartedAt.Format(time.RFC3339Nano), FinishedAt: retained.FinishedAt.Format(time.RFC3339Nano), ObservedAt: retained.Snapshot.ObservedAt.Format(time.RFC3339Nano),
		ExpectedAnchor: anchor, Intent: command,
	})
	if err != nil {
		result.Reason = "observation_unavailable"
		return result, HostedCaptureRecord{}, err
	}
	return result, retained, nil
}

// hostedStandingBaseline uses the first earlier review observation by journal
// position. A later, more convenient baseline must never replace it. Query
// observations and the current artifact cannot establish this causal boundary.
func hostedStandingBaseline(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, current HostedCaptureRecord, currentRef CaptureReference) (*reviewprovider.HostedPublicationBaseline, CaptureReference, error) {
	observations, err := engine.ReadHostedObservations(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return nil, CaptureReference{}, err
	}
	var currentPosition int64
	for _, observation := range observations {
		in := observation.Input
		if in.Kind == "" && in.CaptureID == currentRef.ID && in.CaptureSHA256 == currentRef.SHA256 && in.CaptureBytes == currentRef.Bytes {
			currentPosition = observation.Position
			break
		}
	}
	if currentPosition == 0 {
		return nil, CaptureReference{}, core.Fail("storage_unavailable", "Stored hosted baseline binding is unavailable.", 500)
	}
	for _, observation := range observations {
		if observation.Position >= currentPosition {
			break
		}
		in := observation.Input
		if in.Kind != "" {
			continue
		}
		ref := CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}
		record, err := captures.Read(ctx, attempt.ID, ref)
		if err != nil {
			return nil, CaptureReference{}, err
		}
		if observation.ProjectID != attempt.ProjectID || observation.AttemptID != attempt.ID || observation.OperationID != attempt.OperationID || in.ExpectedAnchor != *attempt.Anchor || in.Intent != attempt.Intent || record.Intent != current.Intent || record.RepositoryID != current.RepositoryID || record.Repository != current.Repository || record.PullNumber != current.PullNumber || record.PostingActor != current.PostingActor || record.ProviderActor != current.ProviderActor || !record.EarliestPosting.Equal(current.EarliestPosting) || record.StartedAt.Format(time.RFC3339Nano) != in.StartedAt || record.FinishedAt.Format(time.RFC3339Nano) != in.FinishedAt || record.Snapshot.ObservedAt.Format(time.RFC3339Nano) != in.ObservedAt {
			return nil, CaptureReference{}, core.Fail("storage_unavailable", "Stored hosted baseline binding is unavailable.", 500)
		}
		return &reviewprovider.HostedPublicationBaseline{Snapshot: record.Snapshot, StartedAt: record.StartedAt}, ref, nil
	}
	return nil, CaptureReference{}, nil
}
