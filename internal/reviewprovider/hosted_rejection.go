package reviewprovider

import "time"

// The complete snapshot and exact original trigger are already validated by
// ResolveHostedPublication. A rejection proves no completed review or credit.
func publicationRateLimited(snapshot HostedPublicationSnapshot, result HostedPublication, provider Author, closing time.Time) bool {
	inside := func(at time.Time) bool {
		return at.After(result.Trigger.CreatedAt) && (closing.IsZero() || at.Before(closing))
	}
	for _, review := range snapshot.Reviews {
		if publicationActor(review.Author, provider) && (review.State == "pending" || inside(review.SubmittedAt)) {
			return false
		}
	}
	for _, comment := range snapshot.ReviewComments {
		if publicationActor(comment.Author, provider) && (inside(comment.CreatedAt) || inside(comment.UpdatedAt)) {
			return false
		}
	}
	count := 0
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, provider) || !inside(comment.UpdatedAt) {
			continue
		}
		if !inside(comment.CreatedAt) {
			// A standing summary carrying fresh completion proof contradicts a
			// claim that the command was rejected before execution.
			if _, ok := parseStandingZero(comment.Body); ok {
				return false
			}
			continue
		}
		if !standingCommandReply(comment.Body, "⚠️ Action not completed", "Review rate limited.") {
			return false
		}
		count++
	}
	return count == 1
}
