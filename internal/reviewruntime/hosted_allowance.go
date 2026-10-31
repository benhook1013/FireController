package reviewruntime

import (
	"context"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// HostedAllowanceProvider has no review command method. The caller composes it
// from the original operator capability, never a client-selected credential.
type HostedAllowanceProvider interface {
	IssueComments(context.Context, string, int64) ([]reviewprovider.IssueComment, error)
	PostIssueComment(context.Context, string, int64, string) (reviewprovider.IssueComment, error)
}

// ObservedHostedAllowance retains the exact private source reference alongside
// an attributed observation. Neither field grants review execution permission.
// The admission owner must journal the reference and recheck current facts.
type ObservedHostedAllowance struct {
	QueryID     string
	Capture     CaptureReference
	Observation reviewprovider.HostedAllowanceObservation
	Reason      string
}

func sameHostedQueryTarget(current, original reviewexec.Anchor) bool {
	// A fresh observation and a repository rename preserve immutable source
	// identity. Every Git/comparison field must still match the original query.
	current.ObservedAt, current.Repository = original.ObservedAt, original.Repository
	return current == original
}

// ObserveHostedAllowance makes one bounded preflight pass. It journals intent
// before its one fixed non-review POST. Existing intent, including a lost POST
// response, can only be observed; absence never authorizes another post.
// Repeating this function does not poll internally or refresh the query window.
func ObserveHostedAllowance(ctx context.Context, provider HostedAllowanceProvider, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, anchor reviewexec.Anchor, repository reviewprovider.Repository, postingActor, providerActor reviewprovider.Author) (ObservedHostedAllowance, error) {
	result := ObservedHostedAllowance{Reason: "allowance_not_proven"}
	observed, err := time.Parse(time.RFC3339Nano, anchor.ObservedAt)
	if ctx == nil || provider == nil || captures == nil || engine == nil || repository.ID != anchor.RepositoryID || repository.FullName == "" ||
		err != nil || observed.IsZero() || anchor.ObservedAt != observed.UTC().Format(time.RFC3339Nano) || time.Since(observed) > time.Minute || observed.After(time.Now().Add(5*time.Second)) {
		return result, core.Fail("validation_failed", "Use the fresh source and its configured hosted allowance capability.", 400)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	query, err := engine.ReadHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID)
	created := false
	if err != nil {
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			return result, err
		}
		query, created, err = engine.BeginHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID, attempt.Revision, anchor, postingActor, providerActor)
		if err != nil {
			return result, err
		}
	}
	result.QueryID = query.ID
	if query.OperationID != attempt.OperationID || !sameHostedQueryTarget(anchor, query.Anchor) || query.PostingActor.ID != postingActor.ID || query.PostingActor.Kind != postingActor.Kind || query.ProviderActor.ID != providerActor.ID || query.ProviderActor.Kind != providerActor.Kind {
		result.Reason = "query_binding_changed"
		return result, nil
	}
	if created {
		comment, err := provider.PostIssueComment(ctx, repository.FullName, query.Anchor.PullNumber, query.Intent)
		if err != nil {
			result.Reason = "query_post_unproven"
			return result, err
		}
		query, err = engine.ConfirmHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID, query.Revision, comment)
		if err != nil {
			result.Reason = "query_recording_pending"
			return result, err
		}
	}
	started := time.Now().UTC()
	comments, err := provider.IssueComments(ctx, repository.FullName, query.Anchor.PullNumber)
	if err != nil {
		result.Reason = "query_history_incomplete"
		return result, err
	}
	observedAt := time.Now().UTC()
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	if err != nil {
		return result, core.Fail("storage_unavailable", "Stored hosted allowance query is unavailable.", 500)
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: query.AttemptID, HeadSHA: query.Anchor.CandidateSHA}
	snapshot := reviewprovider.HostedPublicationSnapshot{IssueComments: comments, Complete: true, ObservedAt: observedAt}
	result.Capture, err = captures.Save(ctx, query.AttemptID, HostedCaptureRecord{RepositoryID: query.Anchor.RepositoryID, Repository: query.Anchor.Repository, PullNumber: query.Anchor.PullNumber,
		Intent: intent, PostingActor: query.PostingActor, ProviderActor: query.ProviderActor, EarliestPosting: earliest.Truncate(time.Second), StartedAt: started, FinishedAt: time.Now().UTC(), Snapshot: snapshot})
	if err != nil {
		result.Reason = "query_capture_unavailable"
		return result, err
	}
	// Resolve the reference-checked immutable bytes rather than mutable adapter
	// collections, just as final review evidence does.
	retained, err := captures.Read(ctx, query.AttemptID, result.Capture)
	if err != nil {
		result.Reason = "query_capture_unavailable"
		return result, err
	}
	// Index every complete query capture before interpretation, including
	// cooldown, pending and unsupported replies. A held preparation must not
	// orphan its only source pointer or require a filesystem scan on resume.
	_, err = engine.RecordHostedObservation(ctx, attempt.ProjectID, attempt.ID, reviewexec.HostedObservationInput{Kind: "allowance_query", CaptureID: result.Capture.ID, CaptureSHA256: result.Capture.SHA256, CaptureBytes: result.Capture.Bytes,
		StartedAt: retained.StartedAt.Format(time.RFC3339Nano), FinishedAt: retained.FinishedAt.Format(time.RFC3339Nano), ObservedAt: retained.Snapshot.ObservedAt.Format(time.RFC3339Nano), ExpectedAnchor: query.Anchor, Intent: query.Intent})
	if err != nil {
		result.Reason = "query_capture_recording_pending"
		return result, err
	}
	result.Observation, err = resolveRetainedHostedAllowance(ctx, captures, engine, attempt, query, result.Capture, retained)
	if err != nil {
		return result, err
	}
	result.Reason = result.Observation.Reason
	if result.Observation.QueryCommentID == 0 {
		return result, nil
	}
	if query.Comment != nil && query.Comment.ID != result.Observation.QueryCommentID {
		result.Observation = reviewprovider.HostedAllowanceObservation{State: "unproven", Reason: "query_identity_changed"}
		result.Reason = "query_identity_changed"
		return result, nil
	}
	if query.Comment == nil {
		for _, comment := range retained.Snapshot.IssueComments {
			if comment.ID == result.Observation.QueryCommentID {
				_, err = engine.ConfirmHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID, query.Revision, comment)
				if err != nil {
					result.Observation = reviewprovider.HostedAllowanceObservation{State: "unproven", Reason: "query_recording_pending"}
					result.Reason = "query_recording_pending"
					return result, err
				}
				break
			}
		}
	}
	return result, nil
}
