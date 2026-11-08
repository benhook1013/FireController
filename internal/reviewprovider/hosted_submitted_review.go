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

var submittedIgnored = regexp.MustCompile(`^<summary>(?:⛔|` + standingPicture + `) Files ignored due to path filters: (0|[1-9][0-9]{0,4})</summary>$`)
var submittedSelected = regexp.MustCompile(`^<summary>(?:📒|` + standingPicture + `) Files selected for processing: (0|[1-9][0-9]{0,4})</summary>$`)
var submittedNoReviewable = regexp.MustCompile(`^<summary>(?:💤|` + standingPicture + `) Files with no reviewable changes: (0|[1-9][0-9]{0,4})</summary>$`)
var submittedAutofix = regexp.MustCompile(`^- \[ \] <!-- \{"checkboxId":"` + publicationUUID + `"\} --> (?:🪄 )?Fix comments: Fix CodeRabbit comments on this PR$`)
var submittedDecorated = map[string]*regexp.Regexp{
	"ℹ️ Review info":       findingReviewInfoIcon,
	"⚙️ Run configuration": regexp.MustCompile(`^<summary>` + standingPicture + ` Run configuration</summary>$`),
	"📥 Commits":            regexp.MustCompile(`^<summary>` + standingPicture + ` Commits</summary>$`),
}

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
	if !p.take("<details>", false) {
		return false
	}
	line, ok := p.next()
	if !ok || line.quote != 0 || line.hidden || line.text != "<summary>"+label+"</summary>" && (submittedDecorated[label] == nil || !submittedDecorated[label].MatchString(line.text)) {
		return false
	}
	p.at++
	return true
}

// Only the observed optional dl/dd wrapper is accepted, with a matching close.
func (p *submittedReviewParser) wrap() (wrapped, ok bool) {
	if line, _ := p.next(); line.text == "<dl>" {
		if !p.take("<dl>", false) || !p.take("<dd>", false) {
			return true, false
		}
		p.take("<br>", false)
		return true, true
	}
	return false, true
}
func (p *submittedReviewParser) close(wrapped bool) bool {
	return (!wrapped || p.take("</dd>", false) && p.take("</dl>", false)) && p.take("</details>", false)
}

// The caution contains real counted outside findings. Validate it through the
// ordinary finding decoder; it is never discarded as presentation or metadata.
func (p *submittedReviewParser) caution(body string) bool {
	line, present := p.next()
	if !present || line.quote != 1 || line.text != "[!CAUTION]" {
		return true
	}
	if line.hidden {
		return false
	}
	p.at++
	line, ok := p.next()
	if !ok || line.hidden || line.quote != 1 || line.text != "Some comments are outside the diff and can’t be posted inline due to GitHub limitations." {
		return false
	}
	p.at++
	line, ok = p.next()
	if !ok || line.hidden || line.quote != 1 || !findingOutsideHeader.MatchString(line.text) {
		return false
	}
	inventory := HostedFindingInventory{Complete: true, Findings: []HostedFinding{}}
	decoder := findingDecoder{inventory: &inventory, seen: map[string]bool{}}
	p.at = decoder.outside(body, p.lines, p.at, 0) + 1
	return inventory.Complete
}

// filePaths consumes the counted file list after its details heading.
func (p *submittedReviewParser) filePaths(count int, entry *regexp.Regexp) (map[string]bool, bool) {
	wrapped, valid := p.wrap()
	if !valid || count < 0 || count > 10000 {
		return nil, false
	}
	paths := map[string]bool{}
	for {
		if line, _ := p.next(); line.text == "</details>" || wrapped && line.text == "</dd>" {
			break
		}
		line, valid := p.text()
		if !valid {
			return nil, false
		}
		path := entry.FindStringSubmatch(line)
		if path == nil || !validRepoPath(path[1]) || paths[path[1]] || len(paths) >= count {
			return nil, false
		}
		paths[path[1]] = true
	}
	return paths, len(paths) == count && p.close(wrapped)
}

func parseSubmittedReview(body, head string) (run string, count int, selected map[string]bool, ok bool) {
	return parseSubmittedReviewEnvelope(body, head, false, false)
}

func parseSubmittedReviewEnvelope(body, head string, current, marked bool) (run string, count int, selected map[string]bool, ok bool) {
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
	if line, _ := p.next(); !current || line.text != "---" {
		claim, valid := p.text()
		match := publicationActionable.FindStringSubmatch(claim)
		if !valid || match == nil {
			return
		}
		count, _ = strconv.Atoi(match[1])
		if count < 1 || count > 200 || !p.caution(body) {
			return
		}
	}
	if !p.take("---", false) {
		return
	}
	if line, _ := p.next(); line.text == "<!-- autofix_checkbox_start -->" {
		if count == 0 || !p.take(line.text, true) {
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
	if current {
		if !p.currentPrompt(body) || !p.take("<h6>", false) {
			return
		}
	} else if line, found := p.next(); found && !line.hidden && line.quote == 0 && line.text == "<details>" {
		end, complete := findingBlock(lines, p.at, 0)
		label := submittedReviewParser{lines: lines, at: p.at + 1}
		header, visible := label.next()
		if complete && visible && !header.hidden && header.quote == 0 && (header.text == "<summary>🤖 Prompt to fix review comments</summary>" || findingPromptIcon.MatchString(header.text)) {
			p.at = end + 1
			if !p.take("---", false) {
				return
			}
		}
	}
	if !p.block("ℹ️ Review info") {
		return
	}
	infoWrapped, valid := p.wrap()
	if !valid || !p.block("⚙️ Run configuration") {
		return
	}
	configWrapped, valid := p.wrap()
	if !valid {
		return
	}
	prefixes := []string{"- **Configuration used**: ", "- **Review profile**: ", "- **Plan**: "}
	if line, _ := p.next(); strings.HasPrefix(line.text, "- **Source mode**: ") {
		if !p.take("- **Source mode**: Auto", false) {
			return
		}
		prefixes = []string{"- **Repository**: ", "- **Branch**: ", "- **File**: ", "- **Commit**: ", "- **Layers**: ", "- **Review profile**: ", "- **Plan**: "}
	}
	for _, prefix := range prefixes {
		line, valid := p.text()
		value, found := strings.CutPrefix(line, prefix)
		if !valid || !found || !validPlainText(value, 1000) || strings.TrimSpace(value) == "" {
			return
		}
		if prefix == "- **Commit**: " || prefix == "- **File**: " {
			kind := "commit"
			if prefix == "- **File**: " {
				kind = "blob"
			}
			if selectedHead, proven := standingConfigurationHead(value, kind); !proven || selectedHead != head {
				return
			}
		}
	}
	line, valid := p.text()
	uuid := standingRunID.FindStringSubmatch(line)
	if !valid || uuid == nil {
		return
	}
	run = uuid[1]
	if !p.close(configWrapped) || !p.block("📥 Commits") {
		return
	}
	commitsWrapped, valid := p.wrap()
	if !valid {
		return
	}
	line, valid = p.text()
	comparison := hostedComparisonLine.FindStringSubmatch(line)
	if !valid || comparison == nil || strings.Trim(comparison[2], "`") != head || !p.close(commitsWrapped) || !p.take("<details>", false) {
		return
	}
	line, valid = p.text()
	if !valid {
		return
	}
	var ignored map[string]bool
	if match := submittedIgnored.FindStringSubmatch(line); match != nil {
		total, _ := strconv.Atoi(match[1])
		ignored, valid = p.filePaths(total, standingIgnoredFile)
		if !valid || !p.take("<details>", false) {
			return
		}
		line, valid = p.text()
	}
	files := submittedSelected.FindStringSubmatch(line)
	if !valid || files == nil {
		return
	}
	total, _ := strconv.Atoi(files[1])
	selected, valid = p.filePaths(total, standingFile)
	if !valid || total == 0 {
		return
	}
	for path := range selected {
		if ignored[path] {
			return
		}
	}

	if line, _ := p.next(); line.text == "<details>" {
		if !p.take("<details>", false) {
			return
		}
		header, valid := p.text()
		match := submittedNoReviewable.FindStringSubmatch(header)
		if !valid || match == nil {
			return
		}
		expected, _ := strconv.Atoi(match[1])
		wrapped, sound := p.wrap()
		if !sound || !wrapped || expected > len(selected) {
			return
		}
		seen := map[string]bool{}
		for {
			if line, _ := p.next(); line.text == "</dd>" {
				break
			}
			text, valid := p.text()
			path, plain := strings.CutPrefix(text, "* ")
			if !valid || !plain || !validRepoPath(path) || strings.TrimSpace(path) != path || strings.ContainsAny(path, "`<>[]*") || !selected[path] || seen[path] || len(seen) >= expected {
				return
			}
			seen[path] = true
		}
		if len(seen) != expected || !p.close(true) {
			return
		}
	}
	if line, _ := p.next(); standingAvailability.MatchString(line.text) {
		if _, valid = p.text(); !valid {
			return
		}
	}
	if !p.close(infoWrapped) || current && !p.take("</h6>", false) || !p.take("<!-- This is an auto-generated comment by CodeRabbit for review status -->", true) {
		return
	}
	if marked {
		line, valid := p.text()
		if !valid || !publicationMarker.MatchString(line) {
			return
		}
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
	return resolveSubmittedReviewBound(snapshot, result, intent, provider, closing, nil)
}

// A non-nil baseline permits only independently proven terminal publication
// settlement; ordinary extraction retains its submission-time edit boundary.
func resolveSubmittedReviewBound(snapshot HostedPublicationSnapshot, result HostedPublication, intent HostedTriggerIntent, provider Author, closing time.Time, baseline *HostedPublicationSnapshot) (HostedPublication, bool) {
	if len(result.EligibleReviews) != 1 {
		return result, false
	}
	review := result.EligibleReviews[0]
	run, count, selected, valid := parseSubmittedReview(review.Body, intent.HeadSHA)
	if !valid {
		run, count, selected, valid = parseSubmittedReviewEnvelope(review.Body, intent.HeadSHA, true, false)
	}
	if !valid || count == 0 {
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
	if baseline != nil && !submittedSettlementBaseline(*baseline, result, review, *ack, provider, run) {
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
		editBound := !comment.UpdatedAt.After(review.SubmittedAt)
		if baseline != nil {
			editBound = comment.UpdatedAt.Before(ack.UpdatedAt) && comment.CreatedAt.After(result.Trigger.CreatedAt)
			for _, old := range baseline.ReviewComments {
				if old.ID == comment.ID {
					editBound = false
				}
			}
		}
		if !publicationActor(comment.Author, provider) || comment.OriginalHeadSHA != intent.HeadSHA || !inside(comment.CreatedAt) || comment.CreatedAt.After(review.SubmittedAt) || !editBound || strings.TrimSpace(comment.Body) == "" || !selected[comment.Path] {
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
