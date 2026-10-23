package reviewruntime

import (
	"context"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// resolveRetainedHostedAllowance shares the immutable attribution boundary
// between initial observation and both allowance-retention paths. Baselines
// come exclusively from the same attempt's bounded indexed query journal.
func resolveRetainedHostedAllowance(ctx context.Context, captures *HostedCaptureStore, engine *reviewexec.Engine, attempt reviewexec.Attempt, query reviewexec.HostedAllowanceQuery, reference CaptureReference, retained HostedCaptureRecord) (reviewprovider.HostedAllowanceObservation, error) {
	snapshot := func(record HostedCaptureRecord) reviewprovider.HostedAllowanceSnapshot {
		return reviewprovider.HostedAllowanceSnapshot{IssueComments: record.Snapshot.IssueComments, Complete: record.Snapshot.Complete, ObservedAt: record.Snapshot.ObservedAt}
	}
	strict, err := reviewprovider.ResolveHostedAllowance(snapshot(retained), retained.Intent, retained.PostingActor, retained.ProviderActor, retained.EarliestPosting)
	if err != nil || strict.Reason != "query_window_closed" {
		return strict, err
	}
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: query.AttemptID, HeadSHA: query.Anchor.CandidateSHA}
	sameActor := func(a, b reviewprovider.Author) bool { return a.ID == b.ID && a.Kind == b.Kind }
	bound := func(record HostedCaptureRecord) bool {
		return record.Snapshot.Complete && record.Snapshot.IssueComments != nil && record.RepositoryID == query.Anchor.RepositoryID && record.Repository == query.Anchor.Repository && record.PullNumber == query.Anchor.PullNumber && record.Intent == intent && sameActor(record.PostingActor, query.PostingActor) && sameActor(record.ProviderActor, query.ProviderActor) && record.EarliestPosting.Equal(earliest.Truncate(time.Second)) && hostedBaselineQueryPresent(record.Snapshot, query)
	}
	if err != nil || query.Comment == nil || query.OperationID != attempt.OperationID || query.AttemptID != attempt.ID || query.ProjectID != attempt.ProjectID || !bound(retained) || strict.QueryCommentID != query.Comment.ID {
		return strict, nil
	}
	observations, err := engine.ReadHostedObservations(ctx, attempt.ProjectID, attempt.ID)
	if err != nil {
		return strict, err
	}
	matches := func(in reviewexec.HostedObservationInput, record HostedCaptureRecord) bool {
		return in.Kind == "allowance_query" && in.ExpectedAnchor == query.Anchor && in.Intent == query.Intent && in.StartedAt == record.StartedAt.Format(time.RFC3339Nano) && in.FinishedAt == record.FinishedAt.Format(time.RFC3339Nano) && in.ObservedAt == record.Snapshot.ObservedAt.Format(time.RFC3339Nano)
	}
	current := -1
	for index, observation := range observations {
		in := observation.Input
		if in.CaptureID == reference.ID && in.CaptureSHA256 == reference.SHA256 && in.CaptureBytes == reference.Bytes {
			if !matches(in, retained) {
				return strict, nil
			}
			current = index
			break
		}
	}
	if current < 0 {
		return strict, nil
	}
	// The earliest complete query capture is the strongest retained pre-edit
	// candidate. Read at most one private baseline; later captures cannot replace
	// missing, mismatched or unsupported original bytes.
	for index := 0; index < current; index++ {
		in := observations[index].Input
		if in.Kind != "allowance_query" {
			continue
		}
		baseline, err := captures.Read(ctx, attempt.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
		if err != nil {
			return strict, err
		}
		if !bound(baseline) || !matches(in, baseline) {
			return strict, nil
		}
		if baseline.FinishedAt.After(retained.StartedAt) || !baseline.Snapshot.ObservedAt.Before(retained.Snapshot.ObservedAt) {
			return strict, nil
		}
		fallback, err := reviewprovider.ResolveHostedAllowanceWithBaseline(snapshot(retained), snapshot(baseline), intent, query.PostingActor, query.ProviderActor, retained.EarliestPosting)
		if err != nil {
			return strict, err
		}
		return fallback, nil
	}
	return strict, nil
}
