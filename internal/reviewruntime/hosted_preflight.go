package reviewruntime

import (
	"context"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// RetainHostedAllowance derives a preparing-attempt observation from immutable
// source rather than trusting a caller's parsed availability. It records no
// review completion, provider action, attempt transition or execution permit.
func RetainHostedAllowance(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, anchor reviewexec.Anchor, changedFiles int64, reference CaptureReference) error {
	return retainHostedAllowance(ctx, captures, engine, attempt, anchor, changedFiles, reference, false)
}

func RetainHostedCooldown(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, anchor reviewexec.Anchor, changedFiles int64, reference CaptureReference) error {
	return retainHostedAllowance(ctx, captures, engine, attempt, anchor, changedFiles, reference, true)
}

func retainHostedAllowance(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, anchor reviewexec.Anchor, changedFiles int64, reference CaptureReference, cooldown bool) error {
	if ctx == nil || captures == nil || engine == nil || !validHostedReference(reference) {
		return core.Fail("validation_failed", "Use the preparing hosted attempt and its private query capture.", 400)
	}
	query, err := engine.ReadHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return err
	}
	record, err := captures.Read(ctx, attempt.ID, reference)
	if err != nil {
		return err
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: query.AttemptID, HeadSHA: query.Anchor.CandidateSHA}
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	if err != nil || record.RepositoryID != query.Anchor.RepositoryID || record.Repository != query.Anchor.Repository || record.PullNumber != query.Anchor.PullNumber || record.Intent != intent ||
		!sameHostedQueryTarget(anchor, query.Anchor) || record.PostingActor.ID != query.PostingActor.ID || record.PostingActor.Kind != query.PostingActor.Kind || record.ProviderActor.ID != query.ProviderActor.ID || record.ProviderActor.Kind != query.ProviderActor.Kind || !record.EarliestPosting.Equal(earliest.Truncate(time.Second)) {
		return core.Fail("review_held", "The query capture does not match its original hosted source and actors.", 409)
	}
	observation, err := resolveRetainedHostedAllowance(ctx, captures, engine, attempt, query, reference, record)
	if err != nil {
		return err
	}
	want := "available_observed"
	if cooldown {
		want = "cooldown_observed"
	}
	if observation.State != want || query.Comment == nil || observation.QueryCommentID != query.Comment.ID {
		return core.Fail("review_held", "The retained query has no proven available response for this attempt.", 409)
	}
	input := reviewexec.HostedAllowanceInput{QueryID: query.ID, Anchor: anchor,
		ObservedAt: record.Snapshot.ObservedAt.Format(time.RFC3339Nano), ChangedFiles: changedFiles, CaptureID: reference.ID, CaptureSHA256: reference.SHA256, CaptureBytes: reference.Bytes, Observation: observation}
	if cooldown {
		return engine.RecordHostedCooldown(ctx, attempt.ProjectID, attempt.ID, attempt.Revision, input)
	}
	return engine.RecordHostedAllowance(ctx, attempt.ProjectID, attempt.ID, attempt.Revision, input)
}
