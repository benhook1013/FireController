package reviewprovider

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HostedSubmittedReview binds a positive submitted review to its real provider
// run and finished-command reply without inventing publication/attempt IDs.
type HostedSubmittedReview struct {
	ProviderRunID   string
	Acknowledgement IssueComment
}

var submittedSelected = regexp.MustCompile(`^<summary>📒 Files selected for processing: (0|[1-9][0-9]{0,4})</summary>$`)
var submittedAutofix = regexp.MustCompile(`^- \[ \] <!-- \{"checkboxId":"` + publicationUUID + `"\} --> 🪄 Fix comments: Fix CodeRabbit comments on this PR$`)

// Only the observed submitted-review envelope is recognized. In particular,
// prompt text and examples cannot supply any inventory or run metadata.
type submittedReviewParser struct {
	lines []findingLine
	at    int
}

func (p *submittedReviewParser) next() (findingLine, bool) {
	for p.at < len(p.lines) && p.lines[p.at].text == "" {
		p.at++
	}
	if p.at == len(p.lines) {
		return findingLine{}, false
	}
	return p.lines[p.at], true
}
func (p *submittedReviewParser) take(want string, hidden bool) bool {
	line, ok := p.next()
	if !ok || line.quote != 0 || line.hidden != hidden || line.text != want {
		return false
	}
	p.at++
	return true
}
func (p *submittedReviewParser) text() (string, bool) {
	line, ok := p.next()
	if !ok || line.quote != 0 || line.hidden {
		return "", false
	}
	p.at++
	return line.text, true
}
func (p *submittedReviewParser) block(label string) bool {
	return p.take("<details>", false) && p.take("<summary>"+label+"</summary>", false)
}

func parseSubmittedReview(body, head string) (run string, count int, selected map[string]bool, ok bool) {
	if !validBody(body) {
		return
	}
	lines, sound := findingLines(body)
	if !sound {
		return
	}
	p := submittedReviewParser{lines: lines}
	if !p.take("<!-- coderabbit-ai-generated:v1 -->", true) {
		return
	}
	claim, valid := p.text()
	match := publicationActionable.FindStringSubmatch(claim)
	if !valid || match == nil {
		return
	}
	count, _ = strconv.Atoi(match[1])
	if count < 1 || count > 200 || !p.take("---", false) {
		return
	}
	if line, _ := p.next(); line.text == "<!-- autofix_checkbox_start -->" {
		if !p.take(line.text, true) {
			return
		}
		checkbox, found := p.next()
		if !found || checkbox.quote != 0 || !checkbox.hidden || !submittedAutofix.MatchString(checkbox.text) {
			return
		}
		p.at++
		if !p.take("<!-- autofix_checkbox_end -->", true) {
			return
		}
	}
	// The optional prompt is opaque. findingBlock retains visibility across
	// fences/quotes, so examples cannot close the prompt and become metadata.
	if line, found := p.next(); found && !line.hidden && line.quote == 0 && line.text == "<details>" {
		end, complete := findingBlock(lines, p.at, 0)
		label := submittedReviewParser{lines: lines, at: p.at + 1}
		if complete && label.take("<summary>🤖 Prompt to fix review comments</summary>", false) {
			p.at = end + 1
			if !p.take("---", false) {
				return
			}
		}
	}
	if !p.block("ℹ️ Review info") || !p.block("⚙️ Run configuration") {
		return
	}
	for _, prefix := range []string{"- **Configuration used**: ", "- **Review profile**: ", "- **Plan**: "} {
		line, valid := p.text()
		value, found := strings.CutPrefix(line, prefix)
		if !valid || !found || !validPlainText(value, 1000) || strings.TrimSpace(value) == "" {
			return
		}
	}
	line, valid := p.text()
	uuid := standingRunID.FindStringSubmatch(line)
	if !valid || uuid == nil {
		return
	}
	run = uuid[1]
	if !p.take("</details>", false) || !p.block("📥 Commits") {
		return
	}
	line, valid = p.text()
	comparison := hostedComparisonLine.FindStringSubmatch(line)
	if !valid || comparison == nil || strings.Trim(comparison[2], "`") != head || !p.take("</details>", false) || !p.take("<details>", false) {
		return
	}
	line, valid = p.text()
	files := submittedSelected.FindStringSubmatch(line)
	if !valid || files == nil {
		return
	}
	total, _ := strconv.Atoi(files[1])
	if total < 1 || total > 10000 {
		return
	}
	selected = map[string]bool{}
	for {
		line, valid = p.text()
		if !valid {
			return
		}
		if line == "</details>" {
			break
		}
		path := standingFile.FindStringSubmatch(line)
		if path == nil || !validRepoPath(path[1]) || selected[path[1]] || len(selected) >= total {
			return
		}
		selected[path[1]] = true
	}
	if len(selected) != total {
		return
	}
	if line, _ := p.next(); standingAvailability.MatchString(line.text) {
		if _, valid = p.text(); !valid {
			return
		}
	}
	if !p.take("</details>", false) || !p.take("<!-- This is an auto-generated comment by CodeRabbit for review status -->", true) {
		return
	}
	_, extra := p.next()
	return run, count, selected, !extra
}

// An older review may use another envelope. Any visible Run ID claim outside
// known prompt/example scopes conservatively prevents reuse; this check only
// refuses attribution and never supplies positive run authority.
func submittedRunReused(body, run string) bool {
	lines, sound := findingLines(body)
	if !sound {
		return strings.Contains(body, run)
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line.hidden || line.quote != 0 {
			continue
		}
		if line.text == "<details>" {
			end, valid := findingBlock(lines, i, 0)
			if !valid {
				return strings.Contains(body, run)
			}
			switch hostedComparisonLabel(nextFindingText(lines, i+1, end)) {
			case "prompt to fix review comments", "prompt for ai agents", "example":
				i = end
				continue
			}
		}
		if match := standingRunID.FindStringSubmatch(line.text); match != nil && match[1] == run {
			return true
		}
	}
	return false
}

func resolveSubmittedReview(snapshot HostedPublicationSnapshot, result HostedPublication, intent HostedTriggerIntent, provider Author, closing time.Time) (HostedPublication, bool) {
	if len(result.EligibleReviews) != 1 {
		return result, false
	}
	review := result.EligibleReviews[0]
	run, count, selected, valid := parseSubmittedReview(review.Body, intent.HeadSHA)
	if !valid {
		return result, false
	}
	inside := func(at time.Time) bool {
		return !at.Before(result.Trigger.CreatedAt) && (closing.IsZero() || at.Before(closing))
	}
	for _, other := range snapshot.Reviews {
		if !publicationActor(other.Author, provider) {
			continue
		}
		if other.State == "pending" || other.ID != review.ID && (inside(other.SubmittedAt) || submittedRunReused(other.Body, run)) {
			return result, false
		}
	}
	var ack *IssueComment
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, provider) {
			continue
		}
		// An immutable pre-trigger observation of a supported older result
		// proves reuse. The current standing copy may legitimately repeat the
		// submitted run after the trigger and is not rejected by this guard.
		if comment.UpdatedAt.Before(result.Trigger.CreatedAt) {
			if older, known := parseStandingZero(comment.Body); known && older.ProviderRunID == run {
				return result, false
			}
		}
		if !inside(comment.CreatedAt) && !inside(comment.UpdatedAt) {
			continue
		}
		if standingFinishedAck(comment.Body) {
			if ack != nil || !comment.CreatedAt.After(result.Trigger.CreatedAt) || !inside(comment.CreatedAt) || !inside(comment.UpdatedAt) || !comment.UpdatedAt.After(review.SubmittedAt) {
				return result, false
			}
			copy := comment
			ack = &copy
		} else if inside(comment.CreatedAt) {
			return result, false
		}
	}
	if ack == nil {
		return result, false
	}
	roots := []ReviewComment{}
	for _, comment := range snapshot.ReviewComments {
		if comment.InReplyToID != 0 {
			continue
		}
		if comment.ReviewID != review.ID {
			if publicationActor(comment.Author, provider) && (inside(comment.CreatedAt) || inside(comment.UpdatedAt)) {
				return result, false
			}
			continue
		}
		if !publicationActor(comment.Author, provider) || comment.OriginalHeadSHA != intent.HeadSHA || !inside(comment.CreatedAt) || comment.CreatedAt.After(review.SubmittedAt) || comment.UpdatedAt.After(review.SubmittedAt) || strings.TrimSpace(comment.Body) == "" || !selected[comment.Path] {
			return result, false
		}
		copy := comment
		copy.Line = copyPublicationLine(comment.Line)
		copy.StartLine = copyPublicationLine(comment.StartLine)
		copy.OriginalLine = copyPublicationLine(comment.OriginalLine)
		copy.OriginalStartLine = copyPublicationLine(comment.OriginalStartLine)
		roots = append(roots, copy)
	}
	if !hostedInlineCountMatches(roots, count) {
		return result, false
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	result.State, result.Reason = "attributed", "submitted_review_inventory_attributed"
	result.SubmittedReview = &HostedSubmittedReview{ProviderRunID: run, Acknowledgement: *ack}
	result.Batches, result.Roots, result.FinishedAt = []HostedReview{review}, roots, ack.UpdatedAt
	return result, true
}
