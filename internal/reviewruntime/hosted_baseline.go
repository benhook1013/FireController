package reviewruntime

import (
	"context"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// retainHostedBaseline collects every hosted body once after available allowance
// is durably retained. Query-only observations remain diagnostic source; their
// nil review collections cannot prove absence of an earlier standing Run ID.
func retainHostedBaseline(ctx context.Context, provider HostedRecoveryReader, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, anchor reviewexec.Anchor, postingActor, providerActor reviewprovider.Author) (CaptureReference, error) {
	fail := func() (CaptureReference, error) {
		return CaptureReference{}, core.Fail("review_held", "The complete pre-post hosted history is unavailable.", 409)
	}
	if ctx == nil || provider == nil || captures == nil || engine == nil || attempt.State != "preparing" || attempt.Revision != 1 {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	query, err := engine.ReadHostedAllowanceQuery(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return CaptureReference{}, err
	}
	allowance, err := engine.ReadHostedAllowance(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return CaptureReference{}, err
	}
	sameActor := func(a, b reviewprovider.Author) bool { return a.ID == b.ID && a.Kind == b.Kind }
	if query.Comment == nil || query.OperationID != attempt.OperationID || !sameHostedQueryTarget(anchor, query.Anchor) || !sameHostedQueryTarget(anchor, allowance.Anchor) || allowance.QueryID != query.ID || allowance.Observation.State != "available_observed" || allowance.Observation.QueryCommentID != query.Comment.ID || !sameActor(postingActor, query.PostingActor) || !sameActor(providerActor, query.ProviderActor) {
		return fail()
	}
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	if err != nil {
		return fail()
	}
	started := time.Now().UTC()
	snapshot, err := provider.ObserveHosted(ctx, anchor.Repository, anchor.PullNumber)
	if err != nil {
		return CaptureReference{}, err
	}
	if !snapshot.Complete || snapshot.IssueComments == nil || snapshot.Reviews == nil || snapshot.ReviewComments == nil || !hostedBaselineQueryPresent(snapshot, query) {
		return fail()
	}
	record := HostedCaptureRecord{RepositoryID: query.Anchor.RepositoryID, Repository: query.Anchor.Repository, PullNumber: query.Anchor.PullNumber,
		Intent: reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: query.AttemptID, HeadSHA: query.Anchor.CandidateSHA}, PostingActor: query.PostingActor, ProviderActor: query.ProviderActor, EarliestPosting: earliest.Truncate(time.Second), StartedAt: started, FinishedAt: time.Now().UTC(), Snapshot: snapshot}
	ref, err := captures.Save(ctx, attempt.ID, record)
	if err != nil {
		return CaptureReference{}, err
	}
	retained, err := captures.Read(ctx, attempt.ID, ref)
	if err != nil {
		return ref, err
	}
	// Read back immutable bytes before indexing; no parsed availability or second
	// RecordHostedAllowance can replace the already-retained preflight facts.
	if !retained.Snapshot.Complete || retained.Snapshot.IssueComments == nil || retained.Snapshot.Reviews == nil || retained.Snapshot.ReviewComments == nil || !hostedBaselineQueryPresent(retained.Snapshot, query) {
		return ref, core.Fail("review_held", "The complete pre-post hosted history is unavailable.", 409)
	}
	_, err = engine.RecordHostedObservation(ctx, attempt.ProjectID, attempt.ID, reviewexec.HostedObservationInput{Kind: "allowance_query", CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: retained.StartedAt.Format(time.RFC3339Nano), FinishedAt: retained.FinishedAt.Format(time.RFC3339Nano), ObservedAt: retained.Snapshot.ObservedAt.Format(time.RFC3339Nano), ExpectedAnchor: query.Anchor, Intent: query.Intent})
	return ref, err
}

func hostedBaselineQueryPresent(snapshot reviewprovider.HostedPublicationSnapshot, query reviewexec.HostedAllowanceQuery) bool {
	if query.Comment == nil {
		return false
	}
	for _, comment := range snapshot.IssueComments {
		if comment.ID == query.Comment.ID && comment.Body == query.Intent && comment.Author.ID == query.PostingActor.ID && comment.Author.Kind == query.PostingActor.Kind && comment.CreatedAt.Equal(query.Comment.CreatedAt) && comment.UpdatedAt.Equal(query.Comment.UpdatedAt) {
			return true
		}
	}
	return false
}
