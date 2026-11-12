package reviewruntime

import (
	"context"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
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
	// Retention must bind these exact raw bytes to their current indexed query
	// observation. No older source body or Bot-report baseline is required.
	observations, err := engine.ReadHostedObservations(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return err
	}
	matched := false
	for _, observed := range observations {
		in := observed.Input
		if in.CaptureID != reference.ID || in.CaptureSHA256 != reference.SHA256 || in.CaptureBytes != reference.Bytes {
			continue
		}
		matched = in.Kind == "allowance_query" && in.ExpectedAnchor == query.Anchor && in.Intent == query.Intent &&
			in.StartedAt == record.StartedAt.Format(time.RFC3339Nano) && in.FinishedAt == record.FinishedAt.Format(time.RFC3339Nano) && in.ObservedAt == record.Snapshot.ObservedAt.Format(time.RFC3339Nano)
		break
	}
	if !matched {
		return core.Fail("review_held", "The query capture does not match its retained source observation.", 409)
	}
	observation, err := reviewprovider.ResolveHostedAllowance(reviewprovider.HostedAllowanceSnapshot{IssueComments: record.Snapshot.IssueComments, Complete: record.Snapshot.Complete, ObservedAt: record.Snapshot.ObservedAt}, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
	if err != nil {
		return err
	}
	want := "available_observed"
	if cooldown {
		want = "cooldown_observed"
	}
	if observation.State != want || query.Comment == nil || observation.QueryCommentID != query.Comment.ID {
		message := "The retained query has no proven available response for this attempt."
		if cooldown {
			message = "The retained query has no proven cooldown response for this attempt."
		}
		return core.Fail("review_held", message, 409)
	}
	input := reviewexec.HostedAllowanceInput{QueryID: query.ID, Anchor: anchor,
		ObservedAt: record.Snapshot.ObservedAt.Format(time.RFC3339Nano), ChangedFiles: changedFiles, CaptureID: reference.ID, CaptureSHA256: reference.SHA256, CaptureBytes: reference.Bytes, Observation: observation}
	if cooldown {
		return engine.RecordHostedCooldown(ctx, attempt.ProjectID, attempt.ID, attempt.Revision, input)
	}
	return engine.RecordHostedAllowance(ctx, attempt.ProjectID, attempt.ID, attempt.Revision, input)
}

// Retention is an idempotent local write of the same immutable capture. Retry
// transient writer admission failures without issuing another provider query.
// Every retry revalidates the original binding and reconciles an existing row.
func retryAllowanceRetention(ctx context.Context, wait func(context.Context, time.Duration) error, retain func() error) error {
	for pass := 0; ; pass++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := retain()
		if err == nil || pass == 2 || !retryableHostedRetention(err) {
			return err
		}
		if err := wait(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

func retryableHostedRetention(err error) bool {
	if reviewexec.IsInvalidHostedPreparationEvidence(err) {
		return false
	}
	var failure *core.Error
	return errors.As(err, &failure) && (failure.Code == "overloaded" || failure.Code == "storage_unavailable")
}

// RecoverPreparation only replays immutable local source. It never observes or
// posts to the provider and cannot create an execution permit or new attempt.
func (d *HostedDriver) RecoverPreparation(ctx context.Context, operation reviewexec.Operation, attempt reviewexec.Attempt) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	query, err := d.engine.ReadHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		if reviewexec.IsInvalidHostedPreparationEvidence(err) {
			return nil
		}
		if retryableHostedRetention(err) || ctx.Err() != nil {
			return err
		}
		return nil
	}
	observations, err := d.engine.ReadHostedObservations(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		if reviewexec.IsInvalidHostedPreparationEvidence(err) {
			return nil
		}
		return err
	}
	var reference CaptureReference
	for i := len(observations) - 1; i >= 0; i-- {
		in := observations[i].Input
		if in.Kind == "allowance_query" {
			reference = CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}
			break
		}
	}
	if reference.ID == "" {
		return nil
	}
	record, err := d.captures.Read(ctx, attempt.ID, reference)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if retryableHostedRetention(err) {
			return err
		}
		return nil
	}
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	if err != nil {
		return nil
	}
	observation, err := reviewprovider.ResolveHostedAllowance(reviewprovider.HostedAllowanceSnapshot{IssueComments: record.Snapshot.IssueComments, Complete: record.Snapshot.Complete, ObservedAt: record.Snapshot.ObservedAt}, record.Intent, record.PostingActor, record.ProviderActor, earliest)
	if err != nil || observation.State != "cooldown_observed" {
		return nil
	}
	anchor := query.Anchor
	workspace, err := d.manager.Open(ctx, attempt.ID, anchor.CandidateSHA, anchor.BaseSHA, anchor.MergeBaseSHA, anchor.PatchSHA256, anchor.Repository)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if reviewworkspace.IsOperationalError(err) {
			return err
		}
		return nil
	}
	files, err := d.manager.ChangedFiles(ctx, workspace)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if reviewworkspace.IsOperationalError(err) {
			return err
		}
		return nil
	}
	err = RetainHostedCooldown(ctx, d.captures, d.engine, attempt, anchor, int64(len(files)), reference)
	if retryableHostedRetention(err) || ctx.Err() != nil {
		return err
	}
	return nil
}
