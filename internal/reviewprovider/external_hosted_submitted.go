package reviewprovider

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

var externalSubmittedPrompt = regexp.MustCompile(`^<summary>` + standingPicture + ` Prompt to fix all issues</summary>$`)

// The observed archive presentation contains one opaque fenced prompt. It
// cannot supply inventory, status, or run identity.
func (p *submittedReviewParser) archivePrompt(body string) bool {
	if !p.take("<details>", false) {
		return false
	}
	header, ok := p.text()
	if !ok || !externalSubmittedPrompt.MatchString(header) || !p.take("<dl>", false) || !p.take("<dd>", false) {
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

// An omitted count can only describe an empty archive when a single submitted
// review and a unique authenticated terminal reply occupy one exact full-review
// command window. This is inventory corroboration, not native ownership.
func externalOmittedCountCompletions(s HostedPublicationSnapshot, provider Author, windows externalNativeWindows) map[int64]bool {
	completed := map[int64]bool{}
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
		if command.Body != "@coderabbitai full review" || !command.UpdatedAt.Equal(start) || i > 0 && commands[i-1].CreatedAt.Equal(start) {
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
		if !unique || edit >= 0 && !windows.latestEdit[edit].Before(start) || root < len(roots) && !roots[root].After(ack.UpdatedAt) {
			continue
		}
		completed[review.ID] = true
	}
	return completed
}
