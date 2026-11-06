package reviewprovider

import (
	"strings"
	"time"
)

// ResolveHostedStandingComparison proves only the comparison of an already
// decoded positive, explicitly marked publication. Both snapshots remain
// verbatim. The caller must bind baseline to the original complete pre-POST
// journal entry before using this proof; it grants no zero or completion credit.
func ResolveHostedStandingComparison(snapshot, baseline HostedPublicationSnapshot, publication HostedPublication, posting, provider Author, head, parent, merge string) (string, bool) {
	if publication.State != "attributed" || publication.Trigger.State != "adopted" || len(publication.Batches) != 1 || len(publication.Roots) == 0 || len(publication.EligibleReviews) != 1 || !publicationValidActor(posting) || !publicationValidActor(provider) || provider.Kind != "Bot" || !validSHA(head) || !validSHA(parent) || !validSHA(merge) || !snapshot.Complete || !baseline.Complete || snapshot.IssueComments == nil || baseline.IssueComments == nil || snapshot.Reviews == nil || baseline.Reviews == nil || snapshot.ReviewComments == nil || baseline.ReviewComments == nil || !validPublicationSnapshot(snapshot) || !validPublicationSnapshot(baseline) {
		return "", false
	}
	batch := publication.Batches[0]
	if !baseline.ObservedAt.Before(batch.SubmittedAt) {
		return "", false
	}
	scanner := hostedComparisonScanner{head: head, parent: parent, merge: merge}
	if !scanner.body(batch.Body) || scanner.base != "" {
		return "", false
	}
	if batch.HeadSHA != head || !publicationActor(batch.Author, provider) || !publicationInlineInventory(publication) {
		return "", false
	}
	closing := publicationClosingTime(snapshot, publication.Trigger, posting, provider)
	inside := func(at time.Time) bool {
		return at.After(publication.Trigger.CreatedAt) && (closing.IsZero() || at.Before(closing))
	}
	var summary *HostedStandingZero
	var ack *IssueComment
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, provider) || !inside(comment.UpdatedAt) {
			continue
		}
		if parsed, ok := parseStandingMetadata(comment.Body, false); ok {
			if summary != nil || parsed.HeadSHA != head || parsed.BaseSHA != parent && parsed.BaseSHA != merge || comment.UpdatedAt.Before(batch.SubmittedAt) {
				return "", false
			}
			parsed.Summary = comment
			summary = &parsed
		} else if inside(comment.CreatedAt) && standingFinishedAck(comment.Body) {
			if ack != nil || comment.UpdatedAt.Before(batch.SubmittedAt) {
				return "", false
			}
			copy := comment
			ack = &copy
		} else if inside(comment.CreatedAt) {
			return "", false
		}
	}
	if summary == nil || ack == nil {
		return "", false
	}
	// Bind the mutable source identity and require a genuinely new run, including
	// unsupported older envelopes that still visibly claim that same Run ID.
	prior := false
	for _, comment := range baseline.IssueComments {
		if !publicationActor(comment.Author, provider) {
			continue
		}
		if strings.Contains(strings.ToLower(comment.Body), summary.ProviderRunID) {
			return "", false
		}
		if comment.ID == summary.Summary.ID {
			if comment.CreatedAt != summary.Summary.CreatedAt || !comment.UpdatedAt.Before(publication.Trigger.CreatedAt) {
				return "", false
			}
			prior = true
		}
	}
	if !prior {
		return "", false
	}
	for _, review := range baseline.Reviews {
		if publicationActor(review.Author, provider) && strings.Contains(strings.ToLower(review.Body), summary.ProviderRunID) {
			return "", false
		}
	}
	for _, root := range baseline.ReviewComments {
		if publicationActor(root.Author, provider) && strings.Contains(strings.ToLower(root.Body), summary.ProviderRunID) {
			return "", false
		}
	}
	for _, review := range snapshot.Reviews {
		if publicationActor(review.Author, provider) && (review.State == "pending" && review.HeadSHA == head || review.ID != batch.ID && inside(review.SubmittedAt)) {
			return "", false
		}
	}
	for _, root := range publication.Roots {
		if !summary.selectedPaths[root.Path] {
			return "", false
		}
	}
	return summary.BaseSHA, true
}
