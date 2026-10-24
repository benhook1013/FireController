package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

	// Internal attribution fact, never final evidence or completion credit.
	publicationFinished bool
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
	var baseline HostedCaptureRecord
	var baselineReason string
	var baselineErr error
	baselineLoaded := false
	loadBaseline := func() {
		if !baselineLoaded {
			baseline, baselineReason, baselineErr = loadHostedStandingBaseline(ctx, captures, engine, attempt, retained)
			baselineLoaded = true
		}
	}
	if !inventory.Complete && (inventory.Publication.State == "active" || inventory.Publication.State == "uncertain" && (inventory.Reason == "hosted_finished_inventory_unproven" || inventory.Reason == "attribution_window_closed")) && inventory.Publication.Trigger.State == "adopted" {
		loadBaseline()
		if baselineErr != nil {
			result.Reason = "standing_baseline_unproven"
			return result, baselineErr
		}
		if baselineReason == "" {
			fallback, err := reviewprovider.ExtractHostedFindingsWithBaseline(retained.Snapshot, baseline.Snapshot, retained.Intent, retained.PostingActor, retained.ProviderActor, retained.EarliestPosting)
			if err == nil && fallback.Complete {
				inventory = fallback
			}
		}
	}
	if !inventory.Complete || inventory.Publication.State != "attributed" {
		result.publicationFinished = inventory.Publication.State == "attributed" && !inventory.Publication.FinishedAt.IsZero()
		result.Reason = inventory.Reason
		if result.Reason == "" {
			result.Reason = "finding_inventory_incomplete"
		}
		return result, nil
	}
	publication := inventory.Publication
	if publication.StandingZero != nil {
		loadBaseline()
		if baselineErr != nil || baselineReason != "" {
			result.Reason = baselineReason
			if result.Reason == "" {
				result.Reason = "standing_baseline_unproven"
			}
			return result, baselineErr
		}
		if !hostedStandingRunNew(baseline.Snapshot, retained.ProviderActor, publication.StandingZero.ProviderRunID) {
			result.Reason = "standing_run_not_new"
			return result, nil
		}
	}
	base, proven := reviewprovider.ResolveHostedComparison(publication, anchor.CandidateSHA, anchor.ParentSHA, anchor.MergeBaseSHA)
	if !proven {
		result.Reason = "comparison_unproven"
		return result, nil
	}
	review := reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReportedFindings: int64(len(inventory.Findings)), ReviewedFiles: []string{}, Findings: make([]reviewprovider.ParsedFinding, 0, len(inventory.Findings))}
	metadata := &reviewexec.HostedEvidenceMetadata{PublicationID: publication.PublicationID, ProviderAttemptID: publication.ProviderAttemptID, TriggerCommentID: publication.Trigger.CommentID, ReviewIDs: make([]int64, 0, len(publication.Batches)), ObservedBaseSHA: base, Sources: make([]reviewexec.HostedFindingSource, 0, len(inventory.Findings))}
	if standing := publication.StandingZero; standing != nil {
		metadata.StandingSummary = &reviewexec.HostedStandingSummaryMetadata{CommentID: standing.Summary.ID, ProviderRunID: standing.ProviderRunID, UpdatedAt: standing.Summary.UpdatedAt.Format(time.RFC3339Nano)}
	}
	if submitted := publication.SubmittedReview; submitted != nil {
		metadata.SubmittedReview = &reviewexec.HostedSubmittedReviewMetadata{ProviderRunID: submitted.ProviderRunID, AcknowledgementCommentID: submitted.Acknowledgement.ID}
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

// loadHostedStandingBaseline validates the same attempt's complete, immutable
// pre-post query history before either annotation attribution or UUID checks.
// Journal position selects the latest pre-post query; an invalid latest source
// cannot be replaced by older bytes. Current summary timestamps alone cannot
// establish that mutable-source fact. Local fetch timing uses the exact journal
// instant, while provider trigger matching retains its second-level floor.
func loadHostedStandingBaseline(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, retained HostedCaptureRecord) (HostedCaptureRecord, string, error) {
	const unproven = "standing_baseline_unproven"
	empty := HostedCaptureRecord{}
	query, err := engine.ReadHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		var missing *core.Error
		if errors.As(err, &missing) && missing.Code == "not_found" {
			return empty, unproven, nil
		}
		return empty, unproven, err
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: query.AttemptID, HeadSHA: query.Anchor.CandidateSHA}
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	sameActor := func(a, b reviewprovider.Author) bool { return a.ID == b.ID && a.Kind == b.Kind }
	if err != nil || query.Comment == nil || !sameHostedQueryTarget(*attempt.Anchor, query.Anchor) || query.OperationID != attempt.OperationID || intent != retained.Intent || !sameActor(query.PostingActor, retained.PostingActor) || !sameActor(query.ProviderActor, retained.ProviderActor) {
		return empty, unproven, nil
	}
	posting, err := engine.ReadHostedPostingBoundary(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return empty, unproven, err
	}
	if !retained.EarliestPosting.Equal(posting.Truncate(time.Second)) {
		return empty, unproven, nil
	}
	observations, err := engine.ReadHostedObservations(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return empty, unproven, err
	}
	var baseline *HostedCaptureRecord
	for position := len(observations) - 1; position >= 0; position-- {
		in := observations[position].Input
		finish, err := time.Parse(time.RFC3339Nano, in.FinishedAt)
		if in.Kind != "allowance_query" || err != nil || finish.After(posting) {
			continue
		}
		record, err := captures.Read(ctx, attempt.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
		if err != nil {
			return empty, unproven, err
		}
		if !record.Snapshot.Complete || record.Snapshot.IssueComments == nil || record.Snapshot.Reviews == nil || record.Snapshot.ReviewComments == nil || record.RepositoryID != query.Anchor.RepositoryID || record.Repository != query.Anchor.Repository || record.PullNumber != query.Anchor.PullNumber || record.Intent != intent || !sameActor(record.PostingActor, query.PostingActor) || !sameActor(record.ProviderActor, query.ProviderActor) || !record.EarliestPosting.Equal(earliest.Truncate(time.Second)) ||
			in.ExpectedAnchor != query.Anchor || in.Intent != query.Intent || in.StartedAt != record.StartedAt.Format(time.RFC3339Nano) || in.FinishedAt != record.FinishedAt.Format(time.RFC3339Nano) || in.ObservedAt != record.Snapshot.ObservedAt.Format(time.RFC3339Nano) {
			return empty, unproven, nil
		}
		if !hostedBaselineQueryPresent(record.Snapshot, query) {
			return empty, unproven, nil
		}
		baseline = &record
		break
	}
	if baseline == nil {
		return empty, unproven, nil
	}
	return *baseline, "", nil
}

// A provider run identity must be absent from every provider-owned baseline
// body, including the unmodified roots omitted from attribution-only parsing.
func hostedStandingRunNew(baseline reviewprovider.HostedPublicationSnapshot, provider reviewprovider.Author, runID string) bool {
	containsRun := func(author reviewprovider.Author, body string) bool {
		return author.ID == provider.ID && author.Kind == provider.Kind && strings.Contains(strings.ToLower(body), strings.ToLower(runID))
	}
	for _, comment := range baseline.IssueComments {
		if containsRun(comment.Author, comment.Body) {
			return false
		}
	}
	for _, review := range baseline.Reviews {
		if containsRun(review.Author, review.Body) {
			return false
		}
	}
	for _, comment := range baseline.ReviewComments {
		if containsRun(comment.Author, comment.Body) {
			return false
		}
	}
	return true
}
