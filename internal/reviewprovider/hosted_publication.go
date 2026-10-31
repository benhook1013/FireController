package reviewprovider

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HostedPublicationSnapshot is one explicitly complete, repository/PR-scoped
// observation. The caller owns collection completeness and stable target scope.
type HostedPublicationSnapshot struct {
	IssueComments  []IssueComment
	Reviews        []HostedReview
	ReviewComments []ReviewComment
	Complete       bool
	ObservedAt     time.Time
}

// HostedPublication identifies publication batches and their attached root
// inventory only. Even State="attributed" grants no review coverage, finding
// extraction/adjudication, allowance, readiness or merge credit. Bodies remain
// verbatim, including markers and unclassified summary/outside-diff evidence.
type HostedPublication struct {
	State             string
	Reason            string
	Trigger           HostedTriggerRecovery
	PublicationID     string
	ProviderAttemptID string
	Batches           []HostedReview
	Roots             []ReviewComment
	EligibleReviews   []HostedReview
	FinishedAt        time.Time
	StandingZero      *HostedStandingZero
	SubmittedReview   *HostedSubmittedReview
}

const publicationUUID = `[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}`

var publicationMarker = regexp.MustCompile(`^<!-- coderabbit-review-publication v1 publication=(` + publicationUUID + `) attempt=(` + publicationUUID + `) batch=([1-9][0-9]{0,2})/([1-9][0-9]{0,2}) -->$`)
var publicationActionable = regexp.MustCompile(`^\*\*Actionable comments posted: (0|[1-9][0-9]{0,2})\*\*$`)
var publicationHTMLBlockTag = regexp.MustCompile(`(?i)</?(pre|blockquote)(?:[ \t/>]|$)`)
var publicationCommand = regexp.MustCompile(`(?i)@coderabbitai[ \t]+(?:full[ \t]+review|review|resume)(?:[ \t]|$)`)

type publicationBatch struct {
	publication, attempt string
	ordinal, total       int
}

// publicationVisibleLines conservatively excludes fenced/indented code,
// Markdown quotes and HTML pre/blockquote content. It does not rewrite bodies
// or interpret arbitrary HTML as trusted marker syntax.
func publicationVisibleLines(body string) []string {
	var lines []string
	var fence byte
	fenceLength := 0
	pre, quote := 0, 0
	hiddenComment := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if fence != 0 {
			if indent <= 3 {
				n := 0
				for n < len(trimmed) && trimmed[n] == fence {
					n++
				}
				if n >= fenceLength && strings.TrimSpace(trimmed[n:]) == "" {
					fence = 0
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		if indent > 3 || strings.HasPrefix(trimmed, "\t") {
			continue
		}
		if hiddenComment {
			if strings.Contains(line, "-->") {
				hiddenComment = false
			}
			continue
		}
		if start := strings.Index(line, "<!--"); start >= 0 && !publicationMarker.MatchString(line) {
			if !strings.Contains(line[start+4:], "-->") {
				hiddenComment = true
			}
			// An explicit malformed protocol line remains conflicting evidence;
			// unrelated HTML comments cannot make nested lines into markers.
			if strings.Contains(strings.ToLower(line), "coderabbit-review-publication") {
				lines = append(lines, line)
			}
			continue
		}
		tags := publicationHTMLBlockTag.FindAllStringSubmatch(line, -1)
		htmlBlock := pre > 0 || quote > 0 || len(tags) > 0
		for _, tag := range tags {
			depth := &pre
			if strings.EqualFold(tag[1], "blockquote") {
				depth = &quote
			}
			if strings.HasPrefix(tag[0], "</") {
				if *depth > 0 {
					*depth--
				}
			} else {
				*depth++
			}
		}
		if htmlBlock {
			continue
		}
		if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				fence, fenceLength = trimmed[0], n
				continue
			}
		}
		lines = append(lines, line)
	}
	return lines
}

func publicationBatchMarker(body string) (publicationBatch, bool, bool) {
	var batch publicationBatch
	found := false
	for _, line := range publicationVisibleLines(body) {
		if !strings.Contains(strings.ToLower(line), "coderabbit-review-publication") {
			continue
		}
		match := publicationMarker.FindStringSubmatch(line)
		if match == nil || found {
			return batch, false, true
		}
		ordinal, _ := strconv.Atoi(match[3])
		total, _ := strconv.Atoi(match[4])
		if ordinal > total || total > 200 {
			return batch, false, true
		}
		batch = publicationBatch{match[1], match[2], ordinal, total}
		found = true
	}
	return batch, found, false
}

func publicationRootCount(body string) (int, bool) {
	count, matches := 0, 0
	for _, line := range publicationVisibleLines(body) {
		if !strings.Contains(line, "Actionable comments posted:") {
			continue
		}
		match := publicationActionable.FindStringSubmatch(line)
		if match == nil {
			return 0, false
		}
		count, _ = strconv.Atoi(match[1])
		matches++
	}
	return count, matches == 1 && count <= 200
}

func publicationActor(a, b Author) bool { return a.ID == b.ID && a.Kind == b.Kind }
func publicationValidActor(a Author) bool {
	_, err := normalizeActor(githubActor{ID: a.ID, Login: a.Login, Type: a.Kind})
	return err == nil
}

func validPublicationSnapshot(s HostedPublicationSnapshot) bool {
	if len(s.IssueComments) > githubMaxItems || len(s.Reviews) > githubMaxItems || len(s.ReviewComments) > githubMaxItems || len(s.IssueComments)+len(s.Reviews)+len(s.ReviewComments) > githubMaxItems {
		return false
	}
	total := 0
	add := func(body string) bool { total += len(body); return total <= githubMaxAggregate && validBody(body) }
	seen := map[int64]bool{}
	for _, c := range s.IssueComments {
		if c.ID <= 0 || seen[c.ID] || !add(c.Body) || !publicationValidActor(c.Author) || !hostedTriggerTime(c.CreatedAt) || !hostedTriggerTime(c.UpdatedAt) || c.UpdatedAt.Before(c.CreatedAt) || c.UpdatedAt.After(s.ObservedAt) {
			return false
		}
		seen[c.ID] = true
	}
	seen = map[int64]bool{}
	for _, r := range s.Reviews {
		if r.ID <= 0 || seen[r.ID] || !add(r.Body) || !publicationValidActor(r.Author) || !validSHA(r.HeadSHA) {
			return false
		}
		seen[r.ID] = true
		switch r.State {
		case "pending":
			if !r.SubmittedAt.IsZero() {
				return false
			}
		case "commented", "approved", "changes_requested", "dismissed":
			if !hostedTriggerTime(r.SubmittedAt) || r.SubmittedAt.After(s.ObservedAt) {
				return false
			}
		default:
			return false
		}
	}
	seen = map[int64]bool{}
	for _, c := range s.ReviewComments {
		if c.ID <= 0 || seen[c.ID] || c.ReviewID <= 0 || c.InReplyToID < 0 || c.InReplyToID == c.ID || !add(c.Body) || !publicationValidActor(c.Author) || !validRepoPath(c.Path) || !validSHA(c.HeadSHA) || !validSHA(c.OriginalHeadSHA) || !validReviewCommentLocation(c) || !hostedTriggerTime(c.CreatedAt) || !hostedTriggerTime(c.UpdatedAt) || c.UpdatedAt.Before(c.CreatedAt) || c.UpdatedAt.After(s.ObservedAt) {
			return false
		}
		seen[c.ID] = true
	}
	return true
}

// A strict finished-command reply is diagnostic only. It cannot supply an
// inventory, comparison, run identity or completion time. The caller has already
// validated the complete snapshot and original trigger; reuse that resolver's
// closing boundary rather than independently guessing the command window.
func publicationFinishedInventoryUnproven(snapshot HostedPublicationSnapshot, result HostedPublication, provider Author, closing time.Time) bool {
	inside := func(at time.Time) bool {
		return at.After(result.Trigger.CreatedAt) && (closing.IsZero() || at.Before(closing))
	}
	for _, review := range snapshot.Reviews {
		if publicationActor(review.Author, provider) && review.State == "pending" {
			return false
		}
	}
	count := 0
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, provider) || !inside(comment.UpdatedAt) {
			continue
		}
		if standingFinishedAck(comment.Body) {
			if !inside(comment.CreatedAt) {
				return false
			}
			count++
		} else if inside(comment.CreatedAt) {
			// A different fresh provider reply may be an unknown command outcome.
			// Older mutable standing comments are allowed to remain unsupported.
			return false
		}
	}
	return count == 1
}

// ResolveHostedPublication performs no I/O and does not initiate or retry any
// provider action. Incomplete/ambiguous evidence never proves absence of a post.
func ResolveHostedPublication(snapshot HostedPublicationSnapshot, intent HostedTriggerIntent, postingAuthor, providerAuthor Author, earliestPosting time.Time) (HostedPublication, error) {
	result := HostedPublication{State: "uncertain", Reason: "publication_not_proven", Batches: []HostedReview{}, Roots: []ReviewComment{}, EligibleReviews: []HostedReview{}}
	if !publicationValidActor(providerAuthor) || publicationActor(providerAuthor, postingAuthor) {
		return result, invalidSelector()
	}
	trigger, err := RecoverHostedTrigger(snapshot.IssueComments, snapshot.Complete, intent, postingAuthor, earliestPosting, snapshot.ObservedAt)
	if err != nil {
		return result, err
	}
	result.Trigger = trigger
	if !validPublicationSnapshot(snapshot) {
		result.State, result.Reason = "ambiguous", "invalid_publication_evidence"
		return result, nil
	}
	if trigger.State != "adopted" {
		result.State, result.Reason = trigger.State, trigger.Reason
		return result, nil
	}
	result.State = "active"
	// A later command closes this trigger's attribution window. Edited comments
	// may have removed a command, so their original prose cannot prove attribution.
	var closing time.Time
	for _, comment := range snapshot.IssueComments {
		if comment.ID == trigger.CommentID || publicationActor(comment.Author, providerAuthor) {
			continue
		}
		if !comment.UpdatedAt.Equal(comment.CreatedAt) && !comment.UpdatedAt.Before(trigger.CreatedAt) {
			// The original command may have acted before this edit. Its
			// earliest possible effect is the later of creation and our trigger.
			when := comment.CreatedAt
			if when.Before(trigger.CreatedAt) {
				when = trigger.CreatedAt
			}
			if closing.IsZero() || when.Before(closing) {
				closing = when
			}
			continue
		}
		command := false
		for _, line := range publicationVisibleLines(comment.Body) {
			if publicationCommand.MatchString(line) {
				command = true
				break
			}
		}
		if !command {
			continue
		}
		when := comment.CreatedAt
		if when.Before(trigger.CreatedAt) {
			if comment.UpdatedAt.Before(trigger.CreatedAt) {
				continue
			}
			when = trigger.CreatedAt
		}
		if closing.IsZero() || when.Before(closing) {
			closing = when
		}
	}
	for _, review := range snapshot.Reviews {
		if !publicationActor(review.Author, providerAuthor) || review.HeadSHA != intent.HeadSHA || review.SubmittedAt.IsZero() || !review.SubmittedAt.After(trigger.CreatedAt) || review.State != "commented" && review.State != "approved" && review.State != "changes_requested" {
			continue
		}
		if !closing.IsZero() && !review.SubmittedAt.Before(closing) {
			continue
		}
		result.EligibleReviews = append(result.EligibleReviews, review)
	}
	sort.Slice(result.EligibleReviews, func(i, j int) bool { return result.EligibleReviews[i].ID < result.EligibleReviews[j].ID })
	if publicationRateLimited(snapshot, result, providerAuthor, closing) {
		result.State, result.Reason = "rate_limited", "hosted_review_rate_limited"
		return result, nil
	}
	batches := map[int]HostedReview{}
	total := 0
	for _, review := range result.EligibleReviews {
		marker, found, bad := publicationBatchMarker(review.Body)
		if bad {
			result.State, result.Reason = "ambiguous", "conflicting_publication_evidence"
			return result, nil
		}
		if !found {
			continue
		}
		if total != 0 && (marker.publication != result.PublicationID || marker.attempt != result.ProviderAttemptID || marker.total != total) {
			result.State, result.Reason = "ambiguous", "conflicting_publication_evidence"
			return result, nil
		}
		if _, duplicate := batches[marker.ordinal]; duplicate {
			result.State, result.Reason = "ambiguous", "conflicting_publication_evidence"
			return result, nil
		}
		result.PublicationID, result.ProviderAttemptID, total = marker.publication, marker.attempt, marker.total
		batches[marker.ordinal] = review
	}
	if total == 0 {
		if submitted, proven := resolveSubmittedReview(snapshot, result, intent, providerAuthor, closing); proven {
			return submitted, nil
		}
		result = resolveStandingZero(snapshot, result, intent, providerAuthor, closing)
		if result.State == "attributed" || result.State == "ambiguous" {
			return result, nil
		}
		if publicationFinishedInventoryUnproven(snapshot, result, providerAuthor, closing) {
			result.State, result.Reason = "uncertain", "hosted_finished_inventory_unproven"
			return result, nil
		}
		if !closing.IsZero() {
			result.State, result.Reason = "uncertain", "attribution_window_closed"
		}
		return result, nil
	}
	for ordinal := 1; ordinal <= total; ordinal++ {
		if batch, ok := batches[ordinal]; ok {
			result.Batches = append(result.Batches, batch)
		}
	}
	reviewIDs := map[int64]bool{}
	for _, review := range result.Batches {
		reviewIDs[review.ID] = true
	}
	for _, comment := range snapshot.ReviewComments {
		if !reviewIDs[comment.ReviewID] || comment.InReplyToID != 0 {
			continue
		}
		copy := comment
		copy.Line = copyPublicationLine(comment.Line)
		copy.StartLine = copyPublicationLine(comment.StartLine)
		copy.OriginalLine = copyPublicationLine(comment.OriginalLine)
		copy.OriginalStartLine = copyPublicationLine(comment.OriginalStartLine)
		result.Roots = append(result.Roots, copy)
	}
	sort.Slice(result.Roots, func(i, j int) bool { return result.Roots[i].ID < result.Roots[j].ID })
	if len(batches) != total {
		result.Reason = "missing_publication_batches"
		return result, nil
	}
	count, ok := publicationRootCount(batches[1].Body)
	if !ok {
		// A complete outside-only single batch may omit its inline count. Unknown
		// collections, attached roots or fresh orphan roots cannot imply zero roots.
		outsideOnly := total == 1 && len(result.Roots) == 0 && snapshot.ReviewComments != nil
		inside := func(at time.Time) bool {
			return !at.Before(trigger.CreatedAt) && (closing.IsZero() || at.Before(closing))
		}
		for _, comment := range snapshot.ReviewComments {
			if comment.InReplyToID == 0 && !reviewIDs[comment.ReviewID] && publicationActor(comment.Author, providerAuthor) && (inside(comment.CreatedAt) || inside(comment.UpdatedAt)) {
				outsideOnly = false
			}
		}
		if !outsideOnly || !publicationOutsideOnly(result) {
			result.State, result.Reason = "ambiguous", "invalid_actionable_count"
			return result, nil
		}
		count = 0
	}
	for _, review := range result.Batches {
		if review.SubmittedAt.After(result.FinishedAt) {
			result.FinishedAt = review.SubmittedAt
		}
	}
	for _, comment := range result.Roots {
		if !publicationActor(comment.Author, providerAuthor) || comment.OriginalHeadSHA != intent.HeadSHA || comment.CreatedAt.Before(trigger.CreatedAt) || comment.CreatedAt.After(result.FinishedAt) {
			result.State, result.Reason = "ambiguous", "root_binding_mismatch"
			return result, nil
		}
	}
	if !hostedInlineCountMatches(result.Roots, count) {
		result.State, result.Reason = "ambiguous", "root_count_mismatch"
		return result, nil
	}
	result.State, result.Reason = "attributed", "publication_inventory_attributed"
	return result, nil
}

func copyPublicationLine(line *int64) *int64 {
	if line == nil {
		return nil
	}
	value := *line
	return &value
}
