package reviewprovider

import (
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var hostedAddressedFooter = regexp.MustCompile(`^\n\n✅ Addressed in commit ([a-f0-9]{7,40})\n?$`)

// ExtractHostedFindingsWithBaseline preserves strict extraction except for
// proven preexisting bot identifier refreshes and, only for standing zero,
// proven old-root fix annotations.
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
	// body stay untouched. Bot metadata can change a known non-command's clock
	// without creating a second review command; provider finding records remain.
	projected := snapshot
	trigger := original.Publication.Trigger.CreatedAt
	oldComments := make(map[int64]IssueComment, len(baseline.IssueComments))
	for _, comment := range baseline.IssueComments {
		oldComments[comment.ID] = comment
	}
	projected.IssueComments = append([]IssueComment(nil), snapshot.IssueComments...)
	var lastBotEdit time.Time
	for position, comment := range snapshot.IssueComments {
		old, present := oldComments[comment.ID]
		if present && !publicationActor(comment.Author, providerActor) && baselineBotMetadataRefresh(comment, old, baseline.ObservedAt, trigger) {
			projected.IssueComments[position].UpdatedAt = old.UpdatedAt
			if comment.UpdatedAt.After(lastBotEdit) {
				lastBotEdit = comment.UpdatedAt
			}
		}
	}
	if !lastBotEdit.IsZero() {
		botOnly, botErr := ExtractHostedFindings(projected, intent, postingActor, providerActor, earliestPosting)
		if botErr == nil && botOnly.Complete && !lastBotEdit.After(botOnly.Publication.FinishedAt) {
			return botOnly, nil
		}
	}
	// Removing an old provider annotation is still permitted only for proven
	// standing-zero inventory, independently of the bot-only clock projection.
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
	// The strict resolver owns later-command attribution. Requiring every
	// omitted edit no later than its finished acknowledgment keeps annotations
	// inside that proven window without duplicating the closing-window logic.
	if fallbackErr == nil && fallback.Complete && fallback.Publication.StandingZero != nil &&
		!lastOmitted.After(fallback.Publication.FinishedAt) && !lastBotEdit.After(fallback.Publication.FinishedAt) {
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

// Baseline presence, source-owned Bot identity and byte-stable nonidentifier
// prose are all required. Unknown/erased commands and ordinary human edits
// retain the strict attribution-window guard.
func baselineBotMetadataRefresh(current, old IssueComment, observed, trigger time.Time) bool {
	if current.ID != old.ID || current.Author.Kind != "Bot" || !reflect.DeepEqual(current.Author, old.Author) || !current.CreatedAt.Equal(old.CreatedAt) || !current.CreatedAt.Before(trigger) || !old.CreatedAt.Before(trigger) || old.UpdatedAt.After(observed) || !observed.Before(trigger) || !current.UpdatedAt.After(trigger) || !current.UpdatedAt.After(old.UpdatedAt) {
		return false
	}
	if strings.Contains(strings.ToLower(old.Body), "coderabbit") || strings.Contains(strings.ToLower(current.Body), "coderabbit") {
		return false
	}
	return current.Body == old.Body || botIdentifierProjection(current.Body) == botIdentifierProjection(old.Body)
}

// Preserve every nonidentifier byte and Unicode word boundary. Short alphabetic
// words made only of a..f are prose, not opaque identifiers. Decimal tokens are
// bounded; only full 40/64-hex or digit-bearing 7..39-hex tokens are normalized.
func botIdentifierProjection(body string) string {
	var out strings.Builder
	out.Grow(len(body))
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		token := body[start:end]
		decimal, hexadecimal, digit := len(token) <= 64, true, false
		for _, value := range token {
			isDigit := value >= '0' && value <= '9'
			digit = digit || isDigit
			decimal = decimal && isDigit
			hexadecimal = hexadecimal && (isDigit || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F')
		}
		if decimal || hexadecimal && (len(token) == 40 || len(token) == 64 || len(token) >= 7 && len(token) <= 39 && digit) {
			out.WriteString("\x00identifier\x00")
		} else {
			out.WriteString(token)
		}
		start = -1
	}
	for position, value := range body {
		if unicode.IsLetter(value) || unicode.IsNumber(value) || unicode.IsMark(value) || unicode.Is(unicode.Pc, value) {
			if start < 0 {
				start = position
			}
		} else {
			flush(position)
			out.WriteRune(value)
		}
	}
	flush(len(body))
	return out.String()
}
