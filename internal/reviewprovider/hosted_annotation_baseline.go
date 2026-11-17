package reviewprovider

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var hostedAddressedRangeFooter = regexp.MustCompile(`^\n\n✅ Addressed in commits [a-f0-9]{7,40} to ([a-f0-9]{7,40})\n?$`)
var hostedAddressedFooter = regexp.MustCompile(`^\n\n✅ Addressed in commit ([a-f0-9]{7,40})\n?$`)

// ExtractHostedFindingsWithBaseline permits only proven historical annotations
// or positive submitted publication settlement alongside complete inventory.
// A strictly attributed head-change refusal can settle without review credit.
// Unrelated Bot reports use the canonical current-source actor policy.
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
	if settled := extractHostedSettledSubmission(snapshot, baseline, original, intent, postingActor, providerActor); settled.Complete {
		return settled, nil
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

	// Project only proven old-root annotations; raw source stays unchanged.
	projected := snapshot
	trigger := original.Publication.Trigger.CreatedAt
	// Old annotations may not masquerade as new orphan roots. Omission still
	// requires an independently complete result.
	projected.ReviewComments = make([]ReviewComment, 0, len(snapshot.ReviewComments))
	var lastOmitted time.Time
	for _, root := range snapshot.ReviewComments {
		old, present := oldRoots[root.ID]
		previousReview, oldReviewPresent := oldReviews[root.ReviewID]
		currentReview, reviewPresent := reviews[root.ReviewID]
		if present && oldReviewPresent && reviewPresent &&
			root.InReplyToID == 0 && publicationActor(root.Author, providerActor) &&
			root.CreatedAt.Before(trigger) && old.UpdatedAt.Before(trigger) && root.UpdatedAt.After(trigger) &&
			publicationActor(previousReview.Author, providerActor) && hostedTriggerTime(previousReview.SubmittedAt) && previousReview.SubmittedAt.Before(trigger) &&
			previousReview.HeadSHA == root.OriginalHeadSHA && sameHistoricalAnnotationReview(previousReview, currentReview) &&
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
	// A timestamped head-change refusal proves termination without findings.
	// Ignore baseline-proven old annotations only through the refusal time.
	if fallbackErr == nil && fallback.Publication.State == "failed" && fallback.Reason == "hosted_review_skipped" &&
		!fallback.Publication.FinishedAt.IsZero() && !lastOmitted.After(fallback.Publication.FinishedAt) {
		return fallback, nil
	}
	if fallbackErr == nil && !fallback.Complete {
		fallback = extractHostedSettledSubmission(projected, baseline, fallback, intent, postingActor, providerActor)
	}
	summaryOnly := len(fallback.Findings) > 0 && len(fallback.Publication.Roots) == 0 && len(fallback.Publication.Batches) == 1
	for _, finding := range fallback.Findings {
		summaryOnly = summaryOnly && (finding.Kind == "outside_diff" || finding.Kind == "additional")
	}
	// The strict resolver owns later-command attribution. Requiring every
	// omitted edit no later than its finished acknowledgment keeps annotations
	// inside that proven window without duplicating the closing-window logic.
	positiveInline := len(fallback.Findings) > 0 && publicationInlineInventory(fallback.Publication)
	if fallbackErr == nil && fallback.Complete && (fallback.Publication.StandingZero != nil || fallback.Publication.markedZero || summaryOnly || positiveInline) &&
		!lastOmitted.After(fallback.Publication.FinishedAt) {
		return fallback, nil
	}
	return original, nil
}

func sameAnnotatedRoot(current, baseline ReviewComment, head string) bool {
	suffix, ok := strings.CutPrefix(current.Body, baseline.Body)
	footer := hostedAddressedFooter.FindStringSubmatch(suffix)
	if footer == nil {
		// A range footer is only a presentation annotation on an otherwise
		// unchanged historical root. Its endpoint names this candidate; the
		// first endpoint makes no ancestry, fix, or completion claim here.
		footer = hostedAddressedRangeFooter.FindStringSubmatch(suffix)
	}
	// This exact provider status is presentation only, not a claim about a fix
	// commit. The caller still proves the original comparison and independently
	// complete inventory before it may omit this unchanged historical body.
	latestUpdate := suffix == "\n\n✅ Addressed in the latest update" || suffix == "\n\n✅ Addressed in the latest update\n"
	if !ok || !latestUpdate && (footer == nil || !strings.HasPrefix(head, footer[1])) {
		return false
	}
	// GitHub may remap commit_id when later fixes move a historical comment.
	// original_commit_id and all original source/position fields stay exact.
	if !validSHA(current.HeadSHA) || !validSHA(baseline.HeadSHA) {
		return false
	}
	current.Body, current.UpdatedAt, current.HeadSHA = baseline.Body, baseline.UpdatedAt, baseline.HeadSHA
	return reflect.DeepEqual(current, baseline)
}

// CodeRabbit may remove one canonical count heading from an otherwise exact
// historical submitted review when changing its presentation format. This is
// source equivalence for old annotation exclusion, never new review credit.
func sameHistoricalAnnotationReview(baseline, current HostedReview) bool {
	if reflect.DeepEqual(baseline, current) {
		return true
	}
	count, ok := publicationRootCount(baseline.Body)
	if !ok || !publicationCountAbsent(HostedPublication{EligibleReviews: []HostedReview{current}}) {
		return false
	}
	heading := "\n\n**Actionable comments posted: " + strconv.Itoa(count) + "**"
	if strings.Count(baseline.Body, heading) != 1 || strings.Replace(baseline.Body, heading, "", 1) != current.Body {
		return false
	}
	current.Body = baseline.Body
	return reflect.DeepEqual(baseline, current)
}
