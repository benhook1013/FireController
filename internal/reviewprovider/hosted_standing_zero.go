package reviewprovider

import (
	"regexp"
	"strings"
	"time"
)

const standingStart = "<!-- recent_review_start -->"
const standingEnd = "<!-- recent_review_end -->"

var standingRun = regexp.MustCompile("^- [*][*]Run ID[*][*]: `(" + publicationUUID + ")`$")
var standingInvocation = regexp.MustCompile(`^<!-- CodeRabbit review command invocation: v2:[a-f0-9]{64} -->$`)
var standingFileHeading = regexp.MustCompile(`^files (?:ignored due to path filters|selected for processing): (0|[1-9][0-9]{0,3})$`)
var standingAvailability = regexp.MustCompile(`^[*][*]Included review availability:[*][*] This review used your included allowance[.] Your plan provides up to [0-9]{1,6} included reviews? per hour; [0-9]{1,6} remain after this review[.]$`)
var standingContradiction = regexp.MustCompile(`(?i)cr-comment|actionable comments posted|outside diff|duplicate comments|files? (?:not reviewed|not processed|skipped|omitted)|[1-9][0-9]* (?:actionable )?(?:comments|findings|issues)`)
var standingFile = regexp.MustCompile("^[*] `[^`\r\n]+`(?: is excluded by .+)?$")

// standingRecent retains only the unique visible recent section. Historical
// sections, quoted examples and later edits cannot supply its identity or scope.
func standingRecentSection(body string) (string, bool) {
	if strings.Count(body, standingStart) != 1 || strings.Count(body, standingEnd) != 1 || strings.Contains(body, "FC_STANDING_START") || strings.Contains(body, "FC_STANDING_END") {
		return "", false
	}
	marked := strings.ReplaceAll(strings.ReplaceAll(body, standingStart, "FC_STANDING_START"), standingEnd, "FC_STANDING_END")
	lines, valid := findingLines(marked)
	if !valid {
		return "", false
	}
	start, end := -1, -1
	var ancestors []string
	for _, line := range lines {
		if line.hidden || line.quote != 0 {
			continue
		}
		for _, token := range hostedComparisonTokens.FindAllString(line.text, -1) {
			tag := hostedComparisonTag.FindStringSubmatch(token)
			if tag == nil || hostedComparisonVoid(strings.ToLower(tag[1])) {
				continue
			}
			name := strings.ToLower(tag[1])
			if strings.HasPrefix(token, "</") {
				if len(ancestors) == 0 || ancestors[len(ancestors)-1] != name {
					return "", false
				}
				ancestors = ancestors[:len(ancestors)-1]
			} else if !strings.HasSuffix(token, "/>") {
				ancestors = append(ancestors, name)
			}
		}
		if line.text == "FC_STANDING_START" && len(ancestors) == 0 {
			start = line.next
		}
		if line.text == "FC_STANDING_END" && len(ancestors) == 0 {
			end = line.start
		}
	}
	if start < 0 || end < start {
		return "", false
	}
	// Offsets above refer to the sentinel text, so recover the unchanged original
	// section only after proving that both boundaries were visible and ordered.
	begin := strings.Index(body, standingStart) + len(standingStart)
	finish := strings.Index(body, standingEnd)
	if finish < begin {
		return "", false
	}
	recent := body[begin:finish]
	return recent, true
}

func standingRecent(body, head string) (string, string, bool) {
	recent, ok := standingRecentSection(body)
	if !ok {
		return "", "", false
	}
	run, ok := standingRecentFields(recent, head)
	return recent, run, ok
}

type standingFrame struct{ tag, label string }

// This deliberately accepts only the observed zero-result metadata grammar.
// Unrecognized sections/prose cannot silently become an empty finding inventory.
func standingRecentFields(body, head string) (string, bool) {
	var stack []standingFrame
	zero, runs, declarations := 0, 0, 0
	run := ""
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		if strings.TrimSpace(line) != line {
			return "", false
		}
		if strings.HasPrefix(line, "<summary>") && strings.HasSuffix(line, "</summary>") {
			if len(stack) == 0 || stack[len(stack)-1].tag != "details" || stack[len(stack)-1].label != "" {
				return "", false
			}
			label := hostedComparisonLabel(strings.TrimSuffix(strings.TrimPrefix(line, "<summary>"), "</summary>"))
			if label != "recent review info" && label != "run configuration" && label != "commits" && !standingFileHeading.MatchString(label) {
				return "", false
			}
			stack[len(stack)-1].label = label
			continue
		}
		if line == "<details>" || line == "<dl>" || line == "<dd>" {
			tag := strings.Trim(line, "<>")
			if len(stack) > 0 && stack[len(stack)-1].tag == "details" && stack[len(stack)-1].label == "" {
				return "", false
			}
			stack = append(stack, standingFrame{tag: tag})
			continue
		}
		if line == "</details>" || line == "</dl>" || line == "</dd>" {
			if len(stack) == 0 || stack[len(stack)-1].tag != strings.Trim(line, "</>") || stack[len(stack)-1].tag == "details" && stack[len(stack)-1].label == "" {
				return "", false
			}
			stack = stack[:len(stack)-1]
			continue
		}
		label := ""
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].tag == "details" {
				label = stack[i].label
				break
			}
		}
		switch {
		case standingAvailability.MatchString(line):
			// Decoration only: these numbers never become allowance facts.
		case len(stack) == 0 && (line == "No actionable comments were generated in the recent review. 🎉" || line == "No actionable comments were generated in the recent review."):
			zero++
		case label == "run configuration":
			if standingContradiction.MatchString(line) {
				return "", false
			}
			if match := standingRun.FindStringSubmatch(line); match != nil {
				runs++
				run = match[1]
			} else if !strings.HasPrefix(line, "- **Configuration used**: ") && !strings.HasPrefix(line, "- **Review profile**: ") && !strings.HasPrefix(line, "- **Plan**: ") {
				return "", false
			}
		case label == "commits":
			match := hostedComparisonLine.FindStringSubmatch(line)
			if match == nil || strings.Trim(match[2], "`") != head {
				return "", false
			}
			declarations++
		case standingFileHeading.MatchString(label) && standingFile.MatchString(line):
		default:
			return "", false
		}
	}
	return run, len(stack) == 0 && zero == 1 && runs == 1 && declarations == 1
}

func standingFinished(body string) bool {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 1 {
		return lines[0] == "Full review finished."
	}
	if len(lines) < 5 || lines[0] != "<!-- This is an auto-generated reply by CodeRabbit -->" {
		return false
	}
	lines = lines[1:]
	if len(lines) > 0 && standingInvocation.MatchString(lines[0]) {
		lines = lines[1:]
	}
	return len(lines) == 4 && lines[0] == "<details>" && lines[1] == "<summary>✅ Action performed</summary>" && lines[2] == "Full review finished." && lines[3] == "</details>"
}

func resolveHostedStandingZero(result *HostedPublication, s HostedPublicationSnapshot, intent HostedTriggerIntent, poster, bot Author, earliest, closing time.Time, baseline *HostedPublicationBaseline) bool {
	// Nil collections do not claim that these inventories were fetched. Marked
	// batch compatibility is unchanged; this additional source needs all three.
	if s.IssueComments == nil || s.Reviews == nil || s.ReviewComments == nil {
		return false
	}
	touched := func(created, updated time.Time) bool {
		return (!created.Before(result.Trigger.CreatedAt) && (closing.IsZero() || created.Before(closing))) || (!updated.Before(result.Trigger.CreatedAt) && (closing.IsZero() || updated.Before(closing)))
	}
	var summaries, finishes []IssueComment
	for _, c := range s.IssueComments {
		if !publicationActor(c.Author, bot) || !touched(c.CreatedAt, c.UpdatedAt) {
			continue
		}
		if strings.Contains(c.Body, "recent_review_start") {
			summaries = append(summaries, c)
		}
		if standingFinished(c.Body) {
			finishes = append(finishes, c)
		}
	}
	if len(summaries) == 0 {
		return false
	}
	result.Reason = "standing_zero_unproven"
	if len(summaries) != 1 || len(finishes) != 1 {
		return true
	}
	summary, finish := summaries[0], finishes[0]
	if summary.ID == finish.ID || summary.ID == result.Trigger.CommentID || finish.ID == result.Trigger.CommentID || !finish.CreatedAt.After(result.Trigger.CreatedAt) || !summary.UpdatedAt.After(result.Trigger.CreatedAt) || summary.UpdatedAt.After(finish.UpdatedAt) || !closing.IsZero() && (!summary.UpdatedAt.Before(closing) || !finish.UpdatedAt.Before(closing)) {
		return true
	}
	recent, run, ok := standingRecent(summary.Body, intent.HeadSHA)
	if !ok {
		return true
	}
	for _, c := range s.IssueComments {
		if publicationActor(c.Author, bot) && c.ID != summary.ID && c.ID != finish.ID && touched(c.CreatedAt, c.UpdatedAt) {
			return true
		}
	}
	for _, r := range s.Reviews {
		if publicationActor(r.Author, bot) && (r.SubmittedAt.IsZero() || touched(r.SubmittedAt, r.SubmittedAt)) {
			return true
		}
	}
	for _, c := range s.ReviewComments {
		if publicationActor(c.Author, bot) && touched(c.CreatedAt, c.UpdatedAt) {
			return true
		}
	}
	if baseline == nil {
		result.Reason = "standing_zero_baseline_missing"
		return true
	}
	if reason := standingBaseline(*baseline, summary, run, intent, poster, bot, earliest, closing, result.Trigger); reason != "" {
		result.Reason = reason
		return true
	}
	result.StandingZero = &HostedStandingZero{CommentID: summary.ID, CompletionCommentID: finish.ID, RunID: run, RecentReviewBody: recent}
	result.FinishedAt = finish.UpdatedAt
	result.State, result.Reason = "attributed", "standing_zero_attributed"
	return true
}

func standingBaseline(b HostedPublicationBaseline, current IssueComment, run string, intent HostedTriggerIntent, poster, bot Author, earliest, closing time.Time, trigger HostedTriggerRecovery) string {
	const unproven = "standing_zero_baseline_unproven"
	s := b.Snapshot
	if !s.Complete || s.IssueComments == nil || s.Reviews == nil || s.ReviewComments == nil || !hostedTriggerTime(b.StartedAt) || !hostedTriggerTime(s.ObservedAt) || !b.StartedAt.After(trigger.CreatedAt) || s.ObservedAt.Before(b.StartedAt) || !s.ObservedAt.Before(current.UpdatedAt) || !validPublicationSnapshot(s) {
		return unproven
	}
	original, err := RecoverHostedTrigger(s.IssueComments, s.Complete, intent, poster, earliest, s.ObservedAt)
	if err != nil || original.State != "adopted" || original.CommentID != trigger.CommentID || !original.CreatedAt.Equal(trigger.CreatedAt) {
		return unproven
	}
	for _, c := range s.IssueComments {
		if c.ID == trigger.CommentID || publicationActor(c.Author, bot) {
			continue
		}
		if !c.CreatedAt.Equal(c.UpdatedAt) && !c.UpdatedAt.Before(trigger.CreatedAt) {
			return unproven
		}
		if !c.CreatedAt.Before(trigger.CreatedAt) {
			for _, line := range publicationVisibleLines(c.Body) {
				if publicationCommand.MatchString(line) {
					return unproven
				}
			}
		}
	}
	// Resolve the original window in this snapshot too. A competing command/edit
	// cannot be hidden just because a later observation still contains our trigger.
	prior, err := ResolveHostedPublication(s, intent, poster, bot, earliest)
	if err != nil || prior.Reason == "attribution_window_closed" || prior.State != "active" {
		return unproven
	}
	for _, c := range s.IssueComments {
		if c.ID == current.ID && (!publicationActor(c.Author, bot) || !c.CreatedAt.Equal(current.CreatedAt)) {
			return unproven
		}
		if !publicationActor(c.Author, bot) {
			continue
		}
		if standingFinished(c.Body) && !c.CreatedAt.Before(trigger.CreatedAt) && (closing.IsZero() || c.CreatedAt.Before(closing)) {
			return unproven
		}
		if c.ID != current.ID {
			continue
		}
		previous, present, valid := standingPreviousRun(c.Body)
		if !valid {
			return unproven
		}
		if present && strings.EqualFold(previous, run) {
			return "standing_zero_run_not_new"
		}
	}
	return ""
}

// The baseline may contain findings, but only a uniquely visible canonical Run
// ID in the recent Run configuration supplies the previous identity. Missing
// sections prove absence; malformed/ambiguous sections do not.
func standingPreviousRun(body string) (string, bool, bool) {
	if !strings.Contains(strings.ToLower(body), "recent_review_start") && !strings.Contains(strings.ToLower(body), "recent_review_end") {
		return "", false, true
	}
	recent, ok := standingRecentSection(body)
	if !ok {
		return "", false, false
	}
	lines, ok := findingLines(recent)
	if !ok {
		return "", false, false
	}
	var stack []standingFrame
	runs, run := 0, ""
	for _, l := range lines {
		if l.hidden || l.quote != 0 {
			continue
		}
		line := l.text
		if strings.HasPrefix(line, "<summary>") && strings.HasSuffix(line, "</summary>") {
			if len(stack) == 0 || stack[len(stack)-1].tag != "details" || stack[len(stack)-1].label != "" {
				return "", false, false
			}
			stack[len(stack)-1].label = hostedComparisonLabel(strings.TrimSuffix(strings.TrimPrefix(line, "<summary>"), "</summary>"))
			continue
		}
		if line == "<details>" || line == "<dl>" || line == "<dd>" || line == "<blockquote>" {
			stack = append(stack, standingFrame{tag: strings.Trim(line, "<>")})
			continue
		}
		if line == "</details>" || line == "</dl>" || line == "</dd>" || line == "</blockquote>" {
			if len(stack) == 0 || stack[len(stack)-1].tag != strings.Trim(line, "</>") {
				return "", false, false
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if !strings.Contains(line, "Run ID") {
			continue
		}
		match := standingRun.FindStringSubmatch(line)
		if match == nil {
			return "", false, false
		}
		labels := []string{}
		for _, frame := range stack {
			if frame.tag == "details" {
				labels = append(labels, frame.label)
			} else if frame.tag != "dl" && frame.tag != "dd" {
				return "", false, false
			}
		}
		if len(labels) != 2 || labels[0] != "recent review info" || labels[1] != "run configuration" {
			return "", false, false
		}
		runs++
		run = match[1]
	}
	return run, true, len(stack) == 0 && runs == 1
}
