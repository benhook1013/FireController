package reviewprovider

import (
	"reflect"
	"regexp"
	"strings"
	"time"
)

var hostedAddressedRangeFooter = regexp.MustCompile(`^\n\n✅ Addressed in commits [a-f0-9]{7,40} to ([a-f0-9]{7,40})\n?$`)
var hostedAddressedFooter = regexp.MustCompile(`^\n\n✅ Addressed in commit ([a-f0-9]{7,40})\n?$`)

// ExtractHostedFindingsWithBaseline preserves strict extraction except for
// unrelated bot reports qualified by a complete pre-trigger baseline and,
// for standing zero or complete outside-only results, proven old-root fix annotations.
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

	// This projection is attribution-only. Raw snapshots and every original
	// body stay untouched. Unrelated bot reports can be revised without
	// becoming request authorities; provider finding records remain. Existing
	// reports require both retained bodies to lack even a hidden provider command
	// mention; newly created reports must lack a current mention.
	projected := snapshot
	trigger := original.Publication.Trigger.CreatedAt
	oldComments := make(map[int64]IssueComment, len(baseline.IssueComments))
	for _, comment := range baseline.IssueComments {
		oldComments[comment.ID] = comment
	}
	projected.IssueComments = append([]IssueComment(nil), snapshot.IssueComments...)
	botProjected := false
	for position, comment := range snapshot.IssueComments {
		old, present := oldComments[comment.ID]
		if publicationActor(comment.Author, providerActor) || publicationActor(comment.Author, postingActor) {
			continue
		}
		if present && baselineBotReportRefresh(comment, old, baseline.ObservedAt, trigger) {
			projected.IssueComments[position].UpdatedAt = old.UpdatedAt
			botProjected = true
		} else if !present && baseline.ObservedAt.Before(trigger) && comment.Author.Kind == "Bot" && comment.CreatedAt.After(trigger) && comment.UpdatedAt.After(comment.CreatedAt) && !strings.Contains(strings.ToLower(comment.Body), "coderabbit") {
			// A complete pre-trigger baseline cannot contain a report created
			// later. Its unrelated Bot identity supplies the same non-command
			// report trust as an existing report refresh; retain all raw source.
			projected.IssueComments[position].UpdatedAt = comment.CreatedAt
			botProjected = true
		}
	}
	if botProjected {
		botOnly, botErr := ExtractHostedFindings(projected, intent, postingActor, providerActor, earliestPosting)
		if botErr == nil && (botOnly.Complete || botOnly.Publication.State == "rate_limited") {
			return botOnly, nil
		}
		// A qualified report refresh must not falsely close a still-pending
		// trigger. This changes diagnostics only; no inventory is credited.
		if botErr == nil && original.Reason == "attribution_window_closed" && botOnly.Publication.State == "active" && (botOnly.Reason == "publication_not_proven" || botOnly.Reason == "missing_publication_batches") {
			return botOnly, nil
		}
		// Keep an attributable finished reply visible even when its inventory
		// remains unsupported. A proven unrelated bot edit must not mask that
		// actionable capture refusal as an ordinary provider wait.
		if botErr == nil && original.Reason == "attribution_window_closed" && botOnly.Reason == "hosted_finished_inventory_unproven" {
			original = botOnly
		}
		// Preserve a real inventory refusal after the qualified reports cease
		// closing attribution. It supplies no findings or completion credit.
		if botErr == nil && original.Reason == "attribution_window_closed" && botOnly.Publication.State == "ambiguous" {
			original = botOnly
		}
	}
	// Old annotations may not masquerade as new orphan roots. Omission still
	// requires an independently complete standing-zero or outside-only result.
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
	outsideOnly := len(fallback.Findings) > 0 && len(fallback.Publication.Roots) == 0 && len(fallback.Publication.Batches) == 1
	for _, finding := range fallback.Findings {
		outsideOnly = outsideOnly && finding.Kind == "outside_diff"
	}
	// The strict resolver owns later-command attribution. Requiring every
	// omitted edit no later than its finished acknowledgment keeps annotations
	// inside that proven window without duplicating the closing-window logic.
	if fallbackErr == nil && fallback.Complete && (fallback.Publication.StandingZero != nil || outsideOnly) &&
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
	if !ok || footer == nil || !strings.HasPrefix(head, footer[1]) {
		return false
	}
	current.Body, current.UpdatedAt = baseline.Body, baseline.UpdatedAt
	return reflect.DeepEqual(current, baseline)
}

// Baseline presence and the exact source-owned Bot identity are required.
// Unrelated bot reports are not request authorities; a known report may change
// prose or update after review completion. Either retained body mentioning the
// provider, or any unknown/human/identity-changing edit, keeps the strict guard.
func baselineBotReportRefresh(current, old IssueComment, observed, trigger time.Time) bool {
	return current.ID == old.ID && current.Author.Kind == "Bot" && reflect.DeepEqual(current.Author, old.Author) && current.CreatedAt.Equal(old.CreatedAt) && current.CreatedAt.Before(trigger) && !old.UpdatedAt.After(observed) && observed.Before(trigger) && current.UpdatedAt.After(trigger) && current.UpdatedAt.After(old.UpdatedAt) &&
		!strings.Contains(strings.ToLower(old.Body), "coderabbit") && !strings.Contains(strings.ToLower(current.Body), "coderabbit")
}
