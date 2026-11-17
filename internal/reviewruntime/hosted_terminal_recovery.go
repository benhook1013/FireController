package reviewruntime

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strconv"
	"time"
)

// HostedRetainedDecoder has no provider interface. It interprets only the latest
// original indexed capture, using the same baseline and comparison checks as
// ordinary recovery. Unsupported or corrupt latest bytes never fall back.
func HostedRetainedDecoder(captures *HostedCaptureStore) reviewexec.HostedRetainedDecoder {
	return func(ctx context.Context, engine *reviewexec.Engine, project, attemptID, capabilityID, checksum string) (reviewexec.ReviewEvidenceInput, error) {
		var input reviewexec.ReviewEvidenceInput
		fail := func() (reviewexec.ReviewEvidenceInput, error) {
			return input, core.Fail("hosted_retained_recovery_unproven", "The original retained source does not prove a complete attributed review.", 409)
		}
		bound, err := engine.ReadHostedRecovery(ctx, project, attemptID, capabilityID)
		if err != nil {
			return input, err
		}
		a := bound.Attempt
		if a.State != "failed" || bound.Operation.Reason != reviewexec.HostedFinishedIncompleteReason || bound.EvidenceID != "" {
			return fail()
		}
		observations, err := engine.ReadHostedObservations(ctx, project, a.ID)
		if err != nil {
			return input, err
		}
		if len(observations) == 0 {
			return fail()
		}
		in := observations[len(observations)-1].Input
		if in.Kind != "" || in.CaptureSHA256 != checksum {
			return fail()
		}
		ref := CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}
		record, err := captures.Read(ctx, a.ID, ref)
		if err != nil {
			return input, err
		}
		query, err := engine.ReadHostedAllowanceQuery(ctx, project, a.ID)
		if err != nil {
			return input, err
		}
		anchor := *a.Anchor
		intent := reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA}
		if record.RepositoryID != anchor.RepositoryID || record.Repository != anchor.Repository || record.PullNumber != anchor.PullNumber || record.Intent != intent || !record.EarliestPosting.Equal(bound.EarliestPosting) || record.PostingActor.ID != query.PostingActor.ID || record.PostingActor.Kind != query.PostingActor.Kind || record.ProviderActor.ID != query.ProviderActor.ID || record.ProviderActor.Kind != query.ProviderActor.Kind || in.StartedAt != record.StartedAt.Format(time.RFC3339Nano) || in.FinishedAt != record.FinishedAt.Format(time.RFC3339Nano) || in.ObservedAt != record.Snapshot.ObservedAt.Format(time.RFC3339Nano) {
			return fail()
		}
		trigger, err := reviewprovider.RecoverHostedTrigger(record.Snapshot.IssueComments, record.Snapshot.Complete, intent, record.PostingActor, record.EarliestPosting, record.Snapshot.ObservedAt)
		if err != nil {
			return input, err
		}
		if trigger.State != "adopted" || a.ExternalID != strconv.FormatInt(trigger.CommentID, 10) {
			return fail()
		}
		result, err := deriveHostedEvidence(ctx, captures, engine, a, record, CapturedHosted{Capture: ref, Reason: "capture_retained"}, func(in reviewexec.ReviewEvidenceInput) (reviewexec.Receipt, error) {
			input = in
			return reviewexec.Receipt{}, nil
		})
		if err != nil {
			return reviewexec.ReviewEvidenceInput{}, err
		}
		if !result.Complete {
			return fail()
		}
		return input, nil
	}
}
