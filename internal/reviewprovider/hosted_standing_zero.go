package reviewprovider

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HostedStandingZero is the explicit zero inventory published in a mutable
// standing comment, paired with its attributable finished-command reply.
// It does not invent a GitHub review, publication ID or provider attempt ID.
type HostedStandingZero struct {
	Summary, Acknowledgement        IssueComment
	ProviderRunID, HeadSHA, BaseSHA string
	SelectedFiles, IgnoredFiles     int64
}

var standingRunID = regexp.MustCompile("^- \\*\\*Run ID\\*\\*: `(" + publicationUUID + ")`$")
var standingSelected = regexp.MustCompile(`^<summary><strong>📒 Files selected for processing: (0|[1-9][0-9]{0,4})</strong></summary>$`)
var standingIgnored = regexp.MustCompile(`^<summary><strong>⛔ Files ignored due to path filters: (0|[1-9][0-9]{0,4})</strong></summary>$`)
var standingNoReviewable = regexp.MustCompile(`^<summary><strong>💤 Files with no reviewable changes: (0|[1-9][0-9]{0,4})</strong></summary>$`)
var standingFile = regexp.MustCompile("^\\* `([^`]+)`$")
var standingIgnoredFile = regexp.MustCompile("^\\* `([^`]+)` is excluded by `[^`\\r\\n]+`(?:, `[^`\\r\\n]+`)*$")
var standingCoverage = regexp.MustCompile(`^<!-- final_review_risk_coverage:\{"sourceCommitId":"([a-f0-9]{40})","coveredCommitId":"([a-f0-9]{40})","kind":"reviewed"\} -->$`)
var standingAckInvocation = regexp.MustCompile(`^<!-- CodeRabbit review command invocation: v2:[a-f0-9]{64} -->$`)
var standingAvailability = regexp.MustCompile(`^\*\*Included review availability:\*\* This review used your included allowance\. Your plan provides up to [1-9][0-9]{0,4} included reviews? per hour; (0|[1-9][0-9]{0,4}) remain after this review\.$`)
var standingConfigurationLink = regexp.MustCompile(`^\[([^\[\]\r\n]+)\]\((?:<([^<>\r\n]+)>|([^()<>\r\n]+))\)$`)
var standingFindingClaim = regexp.MustCompile(`(?i)\b(?:comments?|findings?)\s*[:(]\s*[0-9]`)
var standingRiskLabel = regexp.MustCompile(`^<summary><strong>Merge Risk:</strong> <strong>(?:🔵 Low|⚪ Minimal|🟡 Moderate)</strong> · up to <code>([a-f0-9]{5})</code></summary>$`)

const standingRecentStart = "<!-- recent_review_start -->"
const standingRecentEnd = "<!-- recent_review_end -->"

type standingRiskParser struct {
	phase            int
	prefix, coverage string
	complete         bool
}

// line recognizes only the observed root Merge Risk details/dl/dd envelope.
// Returning handled skips tokenizing an authorized protocol comment, while
// arbitrary comments, nested markup and code retain the scanner's visibility.
func (p *standingRiskParser) line(line string, stack []string) (handled, ok bool) {
	if line == "" {
		return false, true
	}
	ancestry := strings.Join(stack, "/")
	if strings.HasPrefix(line, "<!-- final_review_risk_coverage:") {
		proof := standingCoverage.FindStringSubmatch(line)
		if p.phase != 5 || ancestry != "details/dl/dd" || proof == nil || !strings.HasPrefix(proof[1], p.prefix) || proof[1] != proof[2] {
			return false, false
		}
		p.coverage, p.phase = line, 6
		return true, true
	}
	if line == "<!-- final_review_risk_start -->" {
		if p.phase != 4 || ancestry != "details/dl/dd" || p.complete {
			return false, false
		}
		p.phase = 5
		return true, true
	}
	if line == "<!-- final_review_risk_end -->" {
		if p.phase != 6 || ancestry != "details/dl/dd" {
			return false, false
		}
		p.phase = 7
		return true, true
	}
	if strings.Contains(line, "final_review_risk_start") || strings.Contains(line, "final_review_risk_end") {
		return false, false
	}
	switch p.phase {
	case 1:
		label := standingRiskLabel.FindStringSubmatch(line)
		if ancestry == "details" && label != nil {
			p.prefix, p.phase = label[1], 2
		} else {
			p.phase = 0
		}
	case 2:
		if ancestry == "details" && line == "<dl>" {
			p.phase = 3
		} else {
			p.phase = 0
		}
	case 3:
		if ancestry == "details/dl" && line == "<dd>" {
			p.phase = 4
		} else {
			p.phase = 0
		}
	case 4:
		p.phase = 0
	case 5, 6:
		if ancestry == "" {
			return false, false
		}
	case 7:
		if ancestry != "details/dl/dd" || line != "</dd>" {
			return false, false
		}
		p.phase = 8
	case 8:
		if ancestry != "details/dl" || line != "</dl>" {
			return false, false
		}
		p.phase = 9
	case 9:
		if ancestry != "details" || line != "</details>" {
			return false, false
		}
		p.phase, p.complete = 0, true
	}
	if ancestry == "" && line == "<details>" {
		p.phase, p.prefix = 1, ""
	}
	return false, true
}

// standingSections requires visible root recent metadata and the specifically
// labelled Merge Risk coverage envelope. Other HTML containers and examples
// cannot supply protocol proof. Recent metadata has its own closed grammar.
func standingSections(body string) (recent []string, coverage string, ok bool) {
	var stack []string
	var fence byte
	fenceLength := 0
	comment, inRecent := false, false
	starts, ends := 0, 0
	risk := standingRiskParser{}
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if inRecent && line != standingRecentEnd {
			recent = append(recent, line)
		}
		if fence != 0 {
			n := 0
			for n < len(trimmed) && trimmed[n] == fence {
				n++
			}
			if indent <= 3 && n >= fenceLength && strings.TrimSpace(trimmed[n:]) == "" {
				fence = 0
			}
			continue
		}
		pre := len(stack) > 0 && stack[len(stack)-1] == "pre"
		if !comment && !pre {
			if indent > 3 || strings.HasPrefix(trimmed, "\t") || strings.HasPrefix(trimmed, ">") {
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
		}
		if !comment && len(stack) == 0 {
			switch {
			case line == standingRecentStart:
				starts++
				if inRecent || starts != 1 {
					return nil, "", false
				}
				inRecent = true
				continue
			case line == standingRecentEnd:
				ends++
				if !inRecent || ends != 1 {
					return nil, "", false
				}
				inRecent = false
				continue
			case strings.Contains(line, "recent_review_start") || strings.Contains(line, "recent_review_end"):
				return nil, "", false
			}
		}
		if !comment && !pre && !inRecent {
			handled, valid := risk.line(line, stack)
			if !valid {
				return nil, "", false
			}
			if handled {
				continue
			}
		}
		for _, token := range hostedComparisonTokens.FindAllString(line, -1) {
			if comment {
				if token == "-->" {
					comment = false
				}
				continue
			}
			match := hostedComparisonTag.FindStringSubmatch(token)
			pre = len(stack) > 0 && stack[len(stack)-1] == "pre"
			if pre && (match == nil || strings.ToLower(match[1]) != "pre" || !strings.HasPrefix(token, "</")) {
				continue
			}
			if token == "<!--" {
				comment = true
				continue
			}
			if token == "-->" {
				return nil, "", false
			}
			if match == nil {
				continue
			}
			tag := strings.ToLower(match[1])
			closing := strings.HasPrefix(token, "</")
			if hostedComparisonVoid(tag) {
				if closing {
					return nil, "", false
				}
				continue
			}
			if closing {
				if len(stack) == 0 || stack[len(stack)-1] != tag {
					return nil, "", false
				}
				stack = stack[:len(stack)-1]
			} else if !strings.HasSuffix(token, "/>") {
				stack = append(stack, tag)
			}
		}
	}
	return recent, risk.coverage, starts == 1 && ends == 1 && risk.complete && !inRecent && !comment && fence == 0 && len(stack) == 0
}

type standingRecentParser struct {
	lines []string
	at    int
}

func (p *standingRecentParser) take(want ...string) bool {
	for _, line := range want {
		if p.at >= len(p.lines) || p.lines[p.at] != line {
			return false
		}
		p.at++
	}
	return true
}
func (p *standingRecentParser) block(label string) bool {
	return p.take("<details>", "<summary><strong>"+label+"</strong></summary>", "<dl>", "<dd>")
}
func (p *standingRecentParser) close() bool { return p.take("</dd>", "</dl>", "</details>") }

func (p *standingRecentParser) files(pattern *regexp.Regexp, ignored bool, seen map[string]bool) (int64, bool) {
	if !p.take("<details>") || p.at >= len(p.lines) {
		return 0, false
	}
	match := pattern.FindStringSubmatch(p.lines[p.at])
	if match == nil {
		return 0, false
	}
	p.at++
	count, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || count > 10000 || !ignored && count == 0 || !p.take("<dl>", "<dd>") {
		return 0, false
	}
	var found int64
	for p.at < len(p.lines) && p.lines[p.at] != "</dd>" {
		line := p.lines[p.at]
		p.at++
		file := standingFile.FindStringSubmatch(line)
		if ignored {
			file = standingIgnoredFile.FindStringSubmatch(line)
		}
		if file == nil || !validRepoPath(file[1]) || seen[file[1]] || found >= count {
			return 0, false
		}
		seen[file[1]] = true
		found++
	}
	return count, found == count && p.close()
}

// This optional provider metadata is a counted subset of selected paths. It
// neither reduces SelectedFiles nor claims that every selected file was reviewed.
func (p *standingRecentParser) noReviewableFiles(selected map[string]bool) bool {
	if !p.take("<details>") || p.at >= len(p.lines) {
		return false
	}
	match := standingNoReviewable.FindStringSubmatch(p.lines[p.at])
	if match == nil {
		return false
	}
	p.at++
	count, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || count > 10000 || count > int64(len(selected)) || !p.take("<dl>", "<dd>") {
		return false
	}
	seen := map[string]bool{}
	for p.at < len(p.lines) && p.lines[p.at] != "</dd>" {
		path, plain := strings.CutPrefix(p.lines[p.at], "* ")
		p.at++
		if !plain || !validRepoPath(path) || strings.TrimSpace(path) != path || strings.ContainsAny(path, "`<>[]*") || !selected[path] || seen[path] || int64(len(seen)) >= count {
			return false
		}
		seen[path] = true
	}
	return int64(len(seen)) == count && p.close()
}

// Structured source metadata cannot contradict the independently declared
// comparison head. It is not a substitute for comparison or completion proof.
func standingConfigurationHead(value, kind string) (string, bool) {
	match := standingConfigurationLink.FindStringSubmatch(value)
	if match == nil {
		return "", false
	}
	raw := match[2]
	if raw == "" {
		raw = match[3]
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", false
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 5 || parts[0] != "" || !validRepositorySelector(parts[1]+"/"+parts[2]) || parts[3] != kind || !validSHA(parts[4]) || parts[4] != strings.ToLower(parts[4]) {
		return "", false
	}
	if kind == "commit" {
		if len(parts) != 5 || len(match[1]) < 7 || !strings.HasPrefix(parts[4], match[1]) {
			return "", false
		}
	} else if kind != "blob" || len(parts) < 6 || !validRepoPath(strings.Join(parts[5:], "/")) {
		return "", false
	}
	return parts[4], true
}

func parseStandingZero(body string) (zero HostedStandingZero, ok bool) {
	if !validBody(body) {
		return zero, false
	}
	lines, sound := findingLines(body)
	if !sound {
		return zero, false
	}
	for _, line := range lines {
		if !line.hidden && (strings.Contains(line.text, "cr-comment") || strings.Contains(line.text, "Actionable comments posted:") || standingFindingClaim.MatchString(line.text)) {
			return zero, false
		}
	}
	recent, coverage, valid := standingSections(body)
	if !valid {
		return zero, false
	}
	p := standingRecentParser{}
	for _, line := range recent {
		if line == "" {
			continue
		}
		if line != strings.TrimSpace(line) {
			return zero, false
		}
		p.lines = append(p.lines, line)
	}
	if !p.take("No actionable comments were generated in the recent review. 🎉") || !p.block("ℹ️ Recent review info") || !p.block("⚙️ Run configuration") {
		return zero, false
	}
	prefixes := []string{"- **Configuration used**: ", "- **Review profile**: ", "- **Plan**: "}
	configurationCommit, configurationFile := "", ""
	if p.at < len(p.lines) && strings.HasPrefix(p.lines[p.at], "- **Source mode**: ") {
		// A second observed closed metadata grammar; never skip arbitrary lines
		// or finding sections while searching for the authoritative Run ID.
		if !p.take("- **Source mode**: Auto") {
			return zero, false
		}
		prefixes = []string{"- **Repository**: ", "- **Branch**: ", "- **File**: ", "- **Commit**: ", "- **Layers**: ", "- **Review profile**: ", "- **Plan**: "}
	}
	for _, prefix := range prefixes {
		if p.at >= len(p.lines) || !strings.HasPrefix(p.lines[p.at], prefix) {
			return zero, false
		}
		value := strings.TrimPrefix(p.lines[p.at], prefix)
		if !validPlainText(value, 1000) || strings.TrimSpace(value) == "" {
			return zero, false
		}
		switch prefix {
		case "- **Commit**: ":
			if configurationCommit, valid = standingConfigurationHead(value, "commit"); !valid {
				return zero, false
			}
		case "- **File**: ":
			if configurationFile, valid = standingConfigurationHead(value, "blob"); !valid {
				return zero, false
			}
		}
		p.at++
	}
	if p.at >= len(p.lines) {
		return zero, false
	}
	run := standingRunID.FindStringSubmatch(p.lines[p.at])
	if run == nil {
		return zero, false
	}
	p.at++
	zero.ProviderRunID = run[1]
	if !p.close() || !p.block("📥 Commits") || p.at >= len(p.lines) {
		return zero, false
	}
	comparison := hostedComparisonLine.FindStringSubmatch(p.lines[p.at])
	if comparison == nil {
		return zero, false
	}
	p.at++
	zero.BaseSHA, zero.HeadSHA = strings.Trim(comparison[1], "`"), strings.Trim(comparison[2], "`")
	if configurationCommit != "" && configurationCommit != zero.HeadSHA || configurationFile != "" && configurationFile != zero.HeadSHA {
		return zero, false
	}
	if !p.close() {
		return zero, false
	}
	ignored := map[string]bool{}
	// CodeRabbit omits the ignored-files block when there are no excluded
	// paths. Only the exact next selected header permits that omission; any
	// present or malformed ignored block still goes through strict validation.
	if p.at+1 >= len(p.lines) || p.lines[p.at] != "<details>" || !standingSelected.MatchString(p.lines[p.at+1]) {
		if zero.IgnoredFiles, valid = p.files(standingIgnored, true, ignored); !valid {
			return zero, false
		}
	}
	selected := map[string]bool{}
	if zero.SelectedFiles, valid = p.files(standingSelected, false, selected); !valid {
		return zero, false
	}
	for path := range selected {
		if ignored[path] {
			return zero, false
		}
	}
	if p.at+1 < len(p.lines) && p.lines[p.at] == "<details>" && standingNoReviewable.MatchString(p.lines[p.at+1]) {
		if !p.noReviewableFiles(selected) {
			return zero, false
		}
	}
	if p.at < len(p.lines) && standingAvailability.MatchString(p.lines[p.at]) {
		p.at++
	}
	if !p.close() || p.at != len(p.lines) {
		return zero, false
	}
	proof := standingCoverage.FindStringSubmatch(coverage)
	return zero, proof != nil && proof[1] == zero.HeadSHA && proof[2] == zero.HeadSHA
}

func standingFinishedAck(body string) bool {
	if len(body) > githubMaxText {
		return false
	}
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSuffix(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 7 && lines[0] == "<!-- coderabbit-ai-generated:v1 -->" {
		lines = lines[1:]
	}
	return len(lines) == 6 && lines[0] == "<!-- This is an auto-generated reply by CodeRabbit -->" && standingAckInvocation.MatchString(lines[1]) && lines[2] == "<details>" && lines[3] == "<summary>✅ Action performed</summary>" && lines[4] == "Full review finished." && lines[5] == "</details>"
}

// This single-snapshot resolver establishes inventory attribution only. Neither
// the acknowledgement nor the current Run ID proves that a mutable standing
// comment's Run ID changed from its prior body; retained baseline comparison is
// the caller's responsibility before granting review coverage.
func resolveStandingZero(snapshot HostedPublicationSnapshot, result HostedPublication, intent HostedTriggerIntent, provider Author, closing time.Time) HostedPublication {
	inside := func(at time.Time) bool {
		return at.After(result.Trigger.CreatedAt) && (closing.IsZero() || at.Before(closing))
	}
	// An eligible review or fresh provider thread is substantive or unknown
	// inventory. It cannot disappear into the standing zero-result path.
	for _, review := range snapshot.Reviews {
		if publicationActor(review.Author, provider) && (review.State == "pending" && review.HeadSHA == intent.HeadSHA || inside(review.SubmittedAt)) {
			return result
		}
	}
	for _, comment := range snapshot.ReviewComments {
		if publicationActor(comment.Author, provider) && (inside(comment.CreatedAt) || inside(comment.UpdatedAt)) {
			return result
		}
	}
	var summary *HostedStandingZero
	var ack *IssueComment
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, provider) || !inside(comment.UpdatedAt) {
			continue
		}
		if parsed, ok := parseStandingZero(comment.Body); ok && parsed.HeadSHA == intent.HeadSHA {
			if summary != nil {
				result.State, result.Reason = "ambiguous", "conflicting_standing_zero_evidence"
				return result
			}
			parsed.Summary = comment
			summary = &parsed
		} else if inside(comment.CreatedAt) && standingFinishedAck(comment.Body) {
			if ack != nil {
				result.State, result.Reason = "ambiguous", "conflicting_standing_zero_evidence"
				return result
			}
			copy := comment
			ack = &copy
		} else {
			return result
		}
	}
	if summary == nil || ack == nil || ack.UpdatedAt.Before(summary.Summary.UpdatedAt) {
		return result
	}
	for _, comment := range snapshot.IssueComments {
		if comment.ID == summary.Summary.ID || !publicationActor(comment.Author, provider) {
			continue
		}
		if older, ok := parseStandingZero(comment.Body); ok && older.ProviderRunID == summary.ProviderRunID {
			result.State, result.Reason = "ambiguous", "conflicting_standing_zero_evidence"
			return result
		}
	}
	summary.Acknowledgement = *ack
	result.StandingZero = summary
	result.State, result.Reason, result.FinishedAt = "attributed", "standing_zero_inventory_attributed", ack.UpdatedAt
	return result
}

func validStandingZero(z *HostedStandingZero) bool {
	if z == nil {
		return false
	}
	if z.Summary.ID <= 0 || z.Acknowledgement.ID <= 0 || z.Summary.ID == z.Acknowledgement.ID || !publicationValidActor(z.Summary.Author) || !publicationValidActor(z.Acknowledgement.Author) || !publicationActor(z.Summary.Author, z.Acknowledgement.Author) || !hostedTriggerTime(z.Summary.CreatedAt) || !hostedTriggerTime(z.Summary.UpdatedAt) || !hostedTriggerTime(z.Acknowledgement.CreatedAt) || !hostedTriggerTime(z.Acknowledgement.UpdatedAt) || z.Summary.UpdatedAt.Before(z.Summary.CreatedAt) || z.Acknowledgement.UpdatedAt.Before(z.Acknowledgement.CreatedAt) || z.Acknowledgement.UpdatedAt.Before(z.Summary.UpdatedAt) {
		return false
	}
	parsed, ok := parseStandingZero(z.Summary.Body)
	return ok && parsed.ProviderRunID == z.ProviderRunID && parsed.HeadSHA == z.HeadSHA && parsed.BaseSHA == z.BaseSHA && parsed.SelectedFiles == z.SelectedFiles && parsed.IgnoredFiles == z.IgnoredFiles && standingFinishedAck(z.Acknowledgement.Body)
}
