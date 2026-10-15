package reviewprovider

import (
	"context"
	"net/http"
	"time"
)

const hostedObservationLimit = 15 * time.Second

// ObserveHosted collects only the selected repository/PR's issue comments,
// reviews and review comments. All pages and collections share one finite
// budget. It never posts, retries, selects another target or assesses coverage.
// A failed collection is left nil; earlier fully collected observations remain
// available for diagnosis but must not be treated as complete evidence.
func (g *GitHub) ObserveHosted(ctx context.Context, repository string, number int64) (HostedPublicationSnapshot, error) {
	var snapshot HostedPublicationSnapshot
	if !validRepositorySelector(repository) || !validPullNumber(number) {
		return snapshot, invalidSelector()
	}
	if ctx == nil || ctx.Err() != nil {
		return snapshot, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, hostedObservationLimit)
	defer cancel()

	// Observe publications first and command history last: a competing command
	// preceding any selected publication must be included in its attribution window.
	reviews, err := g.Reviews(ctx, repository, number)
	if err != nil {
		return snapshot, err
	}
	candidate := snapshot
	candidate.Reviews, candidate.ObservedAt = reviews, time.Now().UTC()
	if !validPublicationSnapshot(candidate) {
		return snapshot, invalidResponse(false, http.StatusOK)
	}
	snapshot.Reviews = reviews

	inline, err := g.ReviewComments(ctx, repository, number)
	if err != nil {
		return snapshot, err
	}
	candidate = snapshot
	candidate.ReviewComments, candidate.ObservedAt = inline, time.Now().UTC()
	if !validPublicationSnapshot(candidate) {
		return snapshot, invalidResponse(false, http.StatusOK)
	}
	snapshot.ReviewComments = inline

	comments, err := g.IssueComments(ctx, repository, number)
	if err != nil {
		return snapshot, err
	}
	candidate = snapshot
	candidate.IssueComments, candidate.ObservedAt = comments, time.Now().UTC()
	if !validPublicationSnapshot(candidate) {
		return snapshot, invalidResponse(false, http.StatusOK)
	}
	snapshot.IssueComments = comments
	if ctx.Err() != nil {
		return snapshot, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	snapshot.Complete, snapshot.ObservedAt = true, time.Now().UTC()
	return snapshot, nil
}
