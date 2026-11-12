package reviewprovider

import "time"

// The complete snapshot and exact original trigger are already validated by
// ResolveHostedPublication. A rejection proves no completed review or credit.
func publicationRateLimited(snapshot HostedPublicationSnapshot, result HostedPublication, provider Author, closing time.Time) bool {
	return !publicationCommandRejected(snapshot, result, provider, closing, "Review rate limited.").IsZero()
}

func publicationSkipped(snapshot HostedPublicationSnapshot, result HostedPublication, provider Author, closing time.Time) bool {
	return !publicationCommandRejected(snapshot, result, provider, closing, "Review skipped.").IsZero()
}

func publicationCommandRejected(snapshot HostedPublicationSnapshot, result HostedPublication, provider Author, closing time.Time, outcome string) time.Time {
	inside := func(at time.Time) bool {
		return at.After(result.Trigger.CreatedAt) && (closing.IsZero() || at.Before(closing))
	}
	for _, review := range snapshot.Reviews {
		if publicationActor(review.Author, provider) && (review.State == "pending" || inside(review.SubmittedAt)) {
			return time.Time{}
		}
	}
	for _, comment := range snapshot.ReviewComments {
		if publicationActor(comment.Author, provider) && (inside(comment.CreatedAt) || inside(comment.UpdatedAt)) {
			return time.Time{}
		}
	}
	var refusedAt time.Time
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, provider) || !inside(comment.UpdatedAt) {
			continue
		}
		if !inside(comment.CreatedAt) {
			// A standing summary carrying fresh completion proof contradicts a
			// claim that the command was rejected before execution.
			if _, ok := parseStandingZero(comment.Body); ok {
				return time.Time{}
			}
			continue
		}
		if !standingCommandReply(comment.Body, "⚠️ Action not completed", outcome) &&
			!standingCommandReply(comment.Body, "Action not completed", outcome) {
			return time.Time{}
		}
		if !refusedAt.IsZero() {
			return time.Time{}
		}
		refusedAt = comment.UpdatedAt
	}
	return refusedAt
}
