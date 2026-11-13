package reviewprovider

import "strings"

// The caller binds baseline to the original immutable pre-POST journal entry.
// A complete earlier snapshot proves new review/root/run identity, not the old
// contents of newly created roots. Their final publication boundary is instead
// the unique terminal ACK already proven by resolveSubmittedReviewBound.
func submittedSettlementBaseline(baseline HostedPublicationSnapshot, result HostedPublication, review HostedReview, ack IssueComment, provider Author, run string) bool {
	if !baseline.Complete || baseline.IssueComments == nil || baseline.Reviews == nil || baseline.ReviewComments == nil || !hostedTriggerTime(baseline.ObservedAt) || !baseline.ObservedAt.Before(result.Trigger.CreatedAt) || !validPublicationSnapshot(baseline) {
		return false
	}
	reused := func(author Author, body string) bool {
		return publicationActor(author, provider) && strings.Contains(strings.ToLower(body), run)
	}
	for _, old := range baseline.IssueComments {
		if old.ID == ack.ID || old.ID == result.Trigger.CommentID || reused(old.Author, old.Body) {
			return false
		}
	}
	for _, old := range baseline.Reviews {
		if old.ID == review.ID || reused(old.Author, old.Body) {
			return false
		}
	}
	for _, old := range baseline.ReviewComments {
		if old.ReviewID == review.ID || reused(old.Author, old.Body) {
			return false
		}
	}
	return true
}

func extractHostedSettledSubmission(snapshot, baseline HostedPublicationSnapshot, original HostedFindingInventory, intent HostedTriggerIntent, posting, provider Author) HostedFindingInventory {
	p := original.Publication
	if original.Complete || p.State != "uncertain" || p.Reason != "hosted_finished_inventory_unproven" || p.Trigger.State != "adopted" || p.PublicationID != "" || p.ProviderAttemptID != "" {
		return original
	}
	closing := publicationClosingTime(snapshot, p.Trigger, posting, provider)
	settled, ok := resolveSubmittedReviewBound(snapshot, p, intent, provider, closing, &baseline)
	if !ok {
		return original
	}
	decoded := decodeHostedFindings(settled)
	if !decoded.Complete {
		return original
	}
	return decoded
}
