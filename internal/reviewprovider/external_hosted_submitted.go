package reviewprovider

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

var currentSubmittedPrompt = regexp.MustCompile(`^<summary>` + standingPicture + ` Prompt to fix all issues</summary>$`)

// The current provider presentation contains one opaque fenced prompt. It
// cannot supply inventory, status, or run identity.
func (p *submittedReviewParser) currentPrompt(body string) bool {
	if !p.take("<details>", false) {
		return false
	}
	header, ok := p.text()
	if !ok || !currentSubmittedPrompt.MatchString(header) || !p.take("<dl>", false) || !p.take("<dd>", false) {
		return false
	}
	fence, ok := p.next()
	if !ok || strings.TrimSuffix(body[fence.start:fence.end], "\r") != "```" || !p.take("```", true) {
		return false
	}
	for line, ok := p.next(); ok && line.hidden; line, ok = p.next() {
		p.at++
		if line.quote == 0 && strings.TrimSuffix(body[line.start:line.end], "\r") == "```" {
			return p.close(true)
		}
	}
	return false
}

// currentReviewInfoHeading proves that the exact heading wraps just one
// provider Review info block. Neither tag grants metadata authority on its own.
func currentReviewInfoHeading(lines []findingLine, opening int) (start, closing int, ok bool) {
	p := submittedReviewParser{lines: lines, at: opening}
	if !p.take("<h6>", false) {
		return 0, 0, false
	}
	p.next()
	start = p.at
	if !p.block("ℹ️ Review info") {
		return 0, 0, false
	}
	end, closed := findingBlock(lines, start, 0)
	if !closed {
		return 0, 0, false
	}
	p.at = end + 1
	if !p.take("</h6>", false) {
		return 0, 0, false
	}
	return start, p.at - 1, true
}

// The comparison scanner may treat these two exact presentation tags as
// transparent. It still validates the ordinary metadata scopes inside them.
func currentReviewHeadingOffsets(body string) (int, int) {
	if !strings.Contains(body, "<h6>") || !strings.Contains(body, " Prompt to fix all issues</summary>") {
		return -1, -1
	}
	lines, valid := findingLines(body)
	if !valid {
		return -1, -1
	}
	open, close := -1, -1
	for i, line := range lines {
		if line.hidden || line.quote != 0 || line.text != "<details>" {
			continue
		}
		p := submittedReviewParser{lines: lines, at: i}
		if !p.currentPrompt(body) {
			continue
		}
		p.next()
		if _, end, ok := currentReviewInfoHeading(lines, p.at); ok {
			if open >= 0 {
				return -1, -1
			}
			open, close = lines[p.at].start, lines[end].start
		}
	}
	return open, close
}

// An omitted count can only describe an empty archive when a single submitted
// review and a unique authenticated terminal reply occupy one exact full-review
// command window. A nonzero trigger has already been adopted by native
// recovery, so it restricts the proof to that one exact marked command.
func omittedHostedCountCompletions(s HostedPublicationSnapshot, provider Author, windows externalNativeWindows, trigger int64, closing time.Time) map[int64]time.Time {
	completed := map[int64]time.Time{}
	var commands, replies []IssueComment
	var reviews []HostedReview
	var roots []time.Time
	invocations := map[string]int{}
	for _, c := range s.IssueComments {
		if !publicationIssueClockValid(c, s.ObservedAt) {
			return completed
		}
		if publicationActor(c.Author, provider) {
			if strings.Contains(c.Body, "<!-- CodeRabbit review command invocation:") {
				replies = append(replies, c)
				for _, line := range strings.Split(c.Body, "\n") {
					line = strings.TrimSuffix(line, "\r")
					if standingAckInvocation.MatchString(line) {
						invocations[line]++
					}
				}
			}
		} else if publicationHasReviewCommand(c.Body) {
			commands = append(commands, c)
		}
	}
	for _, r := range s.Reviews {
		if publicationActor(r.Author, provider) {
			if r.SubmittedAt.IsZero() {
				return completed
			}
			reviews = append(reviews, r)
		}
	}
	for _, r := range s.ReviewComments {
		if r.InReplyToID == 0 && publicationActor(r.Author, provider) {
			roots = append(roots, r.CreatedAt)
		}
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].CreatedAt.Before(commands[j].CreatedAt) })
	sort.Slice(replies, func(i, j int) bool { return replies[i].CreatedAt.Before(replies[j].CreatedAt) })
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].SubmittedAt.Before(reviews[j].SubmittedAt) })
	sort.Slice(roots, func(i, j int) bool { return roots[i].Before(roots[j]) })
	latestReply := make([]time.Time, len(replies)+1)
	for i, c := range replies {
		latestReply[i+1] = latestReply[i]
		if c.UpdatedAt.After(latestReply[i+1]) {
			latestReply[i+1] = c.UpdatedAt
		}
	}
	for i, command := range commands {
		start, end := command.CreatedAt, s.ObservedAt
		if i+1 < len(commands) {
			end = commands[i+1].CreatedAt
		}
		if !closing.IsZero() && closing.Before(end) {
			end = closing
		}
		exactCommand := trigger == 0 && command.Body == "@coderabbitai full review" ||
			trigger != 0 && command.ID == trigger && externalNativeTrigger.MatchString(command.Body)
		if !exactCommand || !command.UpdatedAt.Equal(start) || i > 0 && commands[i-1].CreatedAt.Equal(start) {
			continue
		}
		a := sort.Search(len(replies), func(i int) bool { return !replies[i].CreatedAt.Before(start) })
		r := sort.Search(len(reviews), func(i int) bool { return !reviews[i].SubmittedAt.Before(start) })
		if a == len(replies) || r == len(reviews) || !latestReply[a].Before(start) || a+1 < len(replies) && replies[a+1].CreatedAt.Before(end) || r+1 < len(reviews) && reviews[r+1].SubmittedAt.Before(end) {
			continue
		}
		ack, review := replies[a], reviews[r]
		if !start.Before(ack.CreatedAt) || !ack.CreatedAt.Before(review.SubmittedAt) || !review.SubmittedAt.Before(ack.UpdatedAt) || !ack.UpdatedAt.Before(end) || !standingFinishedAck(ack.Body) {
			continue
		}
		unique := false
		for _, line := range strings.Split(ack.Body, "\n") {
			if invocations[strings.TrimSuffix(line, "\r")] == 1 {
				unique = true
			}
		}
		edit := sort.Search(len(windows.edits), func(i int) bool { return windows.edits[i].CreatedAt.After(ack.UpdatedAt) }) - 1
		root := sort.Search(len(roots), func(i int) bool { return !roots[i].Before(start) })
		if !unique || trigger == 0 && edit >= 0 && !windows.latestEdit[edit].Before(start) || root < len(roots) && !roots[root].After(ack.UpdatedAt) {
			continue
		}
		completed[review.ID] = ack.UpdatedAt
	}
	return completed
}

// A marked zero without a count requires the exact provider envelope, no new
// roots or competing result, and an authenticated terminal command reply.
func publicationMarkedZero(snapshot HostedPublicationSnapshot, result HostedPublication, provider Author, closing time.Time) time.Time {
	if len(result.Batches) != 1 || len(result.EligibleReviews) != 1 || len(result.Roots) != 0 || !publicationCountAbsent(result) {
		return time.Time{}
	}
	review := result.Batches[0]
	run, count, _, ok := parseSubmittedReviewEnvelope(review.Body, review.HeadSHA, true, true)
	if !ok || count != 0 {
		return time.Time{}
	}
	for _, other := range snapshot.Reviews {
		if other.ID == review.ID || !publicationActor(other.Author, provider) {
			continue
		}
		marker, found, _ := publicationBatchMarker(other.Body)
		if other.State == "pending" || found && marker.publication == result.PublicationID || submittedRunReused(other.Body, run) {
			return time.Time{}
		}
	}
	return omittedHostedCountCompletions(snapshot, provider, externalNativeWindows{}, result.Trigger.CommentID, closing)[review.ID]
}
