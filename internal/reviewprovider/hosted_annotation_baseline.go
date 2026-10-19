package reviewprovider

import (
	"reflect"
	"regexp"
	"strings"
	"time"
)

var hostedAddressedFooter = regexp.MustCompile(`^\n\n✅ Addressed in commit ([a-f0-9]{7,40})\n?$`)

// ExtractHostedFindingsWithBaseline preserves strict extraction except for an
// explicit standing zero obscured solely by proven old-root fix annotations.
// The caller must independently bind baseline to its immutable complete
// pre-POST capture and reject reused provider run UUIDs. Structural/timestamp
// checks here alone establish neither that journal ownership nor coverage.
func ExtractHostedFindingsWithBaseline(snapshot, baseline HostedPublicationSnapshot, intent HostedTriggerIntent, postingActor, providerActor Author, earliestPosting time.Time) (HostedFindingInventory, error) {
	original, err := ExtractHostedFindings(snapshot, intent, postingActor, providerActor, earliestPosting)
	if err != nil || original.Complete {
		return original, err
	}
	if original.Publication.Trigger.State != "adopted" || !snapshot.Complete || !baseline.Complete ||
		snapshot.IssueComments == nil || snapshot.Reviews == nil || snapshot.ReviewComments == nil ||
		baseline.IssueComments == nil || baseline.Reviews == nil || baseline.ReviewComments == nil ||
		!hostedTriggerTime(baseline.ObservedAt) || !baseline.ObservedAt.Before(snapshot.ObservedAt) ||
		!validPublicationSnapshot(snapshot) || !validPublicationSnapshot(baseline) {
		return original, nil
	}
	oldRoots := make(map[int64]ReviewComment, len(baseline.ReviewComments))
	for _, root := range baseline.ReviewComments {
		oldRoots[root.ID] = root
	}
	oldReviews := make(map[int64]HostedReview, len(baseline.Reviews))
	for _, review := range baseline.Reviews {
		oldReviews[review.ID] = review
	}
	reviews := make(map[int64]HostedReview, len(snapshot.Reviews))
	for _, review := range snapshot.Reviews {
		reviews[review.ID] = review
	}

	// This slice is attribution-only. Both raw snapshots and every original
	// body remain untouched; no captured record is removed from storage.
	projected := snapshot
	projected.ReviewComments = make([]ReviewComment, 0, len(snapshot.ReviewComments))
	trigger := original.Publication.Trigger.CreatedAt
	var lastOmitted time.Time
	for _, root := range snapshot.ReviewComments {
		old, present := oldRoots[root.ID]
		previousReview, oldReviewPresent := oldReviews[root.ReviewID]
		currentReview, reviewPresent := reviews[root.ReviewID]
		if present && oldReviewPresent && reviewPresent &&
			root.InReplyToID == 0 && publicationActor(root.Author, providerActor) &&
			root.CreatedAt.Before(trigger) && old.UpdatedAt.Before(trigger) && root.UpdatedAt.After(trigger) &&
			publicationActor(previousReview.Author, providerActor) && hostedTriggerTime(previousReview.SubmittedAt) && previousReview.SubmittedAt.Before(trigger) &&
			previousReview.HeadSHA == root.OriginalHeadSHA && reflect.DeepEqual(previousReview, currentReview) &&
			sameAnnotatedRoot(root, old, intent.HeadSHA) {
			if root.UpdatedAt.After(lastOmitted) {
				lastOmitted = root.UpdatedAt
			}
			continue
		}
		projected.ReviewComments = append(projected.ReviewComments, root)
	}
	if lastOmitted.IsZero() {
		return original, nil
	}
	fallback, fallbackErr := ExtractHostedFindings(projected, intent, postingActor, providerActor, earliestPosting)
	// The strict resolver owns later-command attribution. Requiring every
	// omitted edit no later than its finished acknowledgment keeps annotations
	// inside that proven window without duplicating the closing-window logic.
	if fallbackErr == nil && fallback.Complete && fallback.Publication.StandingZero != nil &&
		!lastOmitted.After(fallback.Publication.FinishedAt) {
		return fallback, nil
	}
	return original, nil
}

func sameAnnotatedRoot(current, baseline ReviewComment, head string) bool {
	suffix, ok := strings.CutPrefix(current.Body, baseline.Body)
	footer := hostedAddressedFooter.FindStringSubmatch(suffix)
	if !ok || footer == nil || !strings.HasPrefix(head, footer[1]) {
		return false
	}
	current.Body, current.UpdatedAt = baseline.Body, baseline.UpdatedAt
	return reflect.DeepEqual(current, baseline)
}
