package reviewprovider

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HostedFindingInventory describes supported source inventory only. Complete
// grants no coverage, adjudication, allowance, readiness or merge credit.
type HostedFindingInventory struct {
	Publication HostedPublication
	Complete    bool
	Reason      string
	Findings    []HostedFinding
}

type HostedFinding struct {
	SourceKey, Fingerprint, Kind, Title, Detail, Severity, Path, Side string
	ReviewID, CommentID                                               int64
	StartLine, EndLine                                                *int64
}

var findingMarker = regexp.MustCompile(`^<!-- cr-comment:v1:([a-f0-9]{24}) -->$`)
var findingOutsideHeader = regexp.MustCompile(`^\*\*⚠️ Outside diff range comments \((0|[1-9][0-9]*)\)\*\*$`)
var findingDuplicateHeader = regexp.MustCompile(`^<summary>♻️ Duplicate comments \((0|[1-9][0-9]*)\)</summary><blockquote>$`)
var findingGroupHeader = regexp.MustCompile(`^<summary>([^<>]+) \((0|[1-9][0-9]*)\)</summary><blockquote>$`)
var findingOutsideSummary = regexp.MustCompile(`^<summary>(?:<em>(?:` + standingPicture + ` )?([^<>]+)</em>|<strong>(?:` + standingPicture + ` )?([^<>]+)</strong>) · .+ · <code>[^<>]+</code></summary><blockquote>$`)

// A decorative icon changes no metadata identity. Recognize only the observed
// closed picture/img envelope and exact label; never fetch the image or treat
// other labelled blocks as review metadata.
var findingPromptIcon = regexp.MustCompile(`^<summary>` + standingPicture + ` Prompt to fix review comments</summary>$`)
var findingReviewInfoIcon = regexp.MustCompile(`^<summary><picture><img src="https://[^"<>\s]+" width="[1-9][0-9]{0,2}" height="[1-9][0-9]{0,2}" align="absmiddle" alt=""></picture> Review info</summary>$`)
var findingPathLocation = regexp.MustCompile("^`([^`]+):([0-9]+)(?:-([0-9]+))?`$")
var findingRange = regexp.MustCompile("^`([0-9]+)-([0-9]+)`: (.+)$")
var findingTitle = regexp.MustCompile(`^\*\*([^*]+)\*\*$`)
var findingHTML = regexp.MustCompile(`</?(?:details|blockquote)>`)
var findingPre = regexp.MustCompile(`(?i)</?pre(?:[ \t/>]|$)`)
var findingQuotedHTML = regexp.MustCompile(`(?i)</?blockquote(?:[ \t][^<>]*)?>`)
var findingConfirmation = regexp.MustCompile(`^✅ Confirmed as addressed by @[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

type findingLine struct {
	start, end, next, quote int
	text                    string
	hidden                  bool
}

// findingLines interprets quoting without changing source byte offsets. Hidden
// examples never supply markers, titles, severity or HTML structural boundaries.
func findingLines(body string) ([]findingLine, bool) {
	var lines []findingLine
	var fence byte
	fenceLength, fenceQuote, pre := 0, 0, 0
	comment := false
	for offset := 0; offset < len(body); {
		end := strings.IndexByte(body[offset:], '\n')
		next := len(body)
		if end < 0 {
			end = len(body)
		} else {
			end += offset
			next = end + 1
		}
		text := strings.TrimSuffix(body[offset:end], "\r")
		quote := 0
		for {
			trimmed := strings.TrimLeft(text, " ")
			if len(text)-len(trimmed) > 3 || !strings.HasPrefix(trimmed, ">") {
				break
			}
			quote++
			text = strings.TrimPrefix(trimmed[1:], " ")
		}
		l := findingLine{start: offset, end: end, next: next, quote: quote, text: strings.TrimSpace(text)}
		trimmed := strings.TrimLeft(text, " ")
		indent := len(text) - len(trimmed)
		if quote > 1 {
			l.hidden = true
		} else if fence != 0 {
			l.hidden = true
			n := 0
			for n < len(trimmed) && trimmed[n] == fence {
				n++
			}
			if quote == fenceQuote && indent <= 3 && n >= fenceLength && strings.TrimSpace(trimmed[n:]) == "" {
				fence = 0
			}
		} else if indent > 3 || strings.HasPrefix(trimmed, "\t") {
			l.hidden = true
		} else if pre > 0 || findingPre.MatchString(trimmed) {
			l.hidden = true
			for _, tag := range findingPre.FindAllString(trimmed, -1) {
				if strings.HasPrefix(tag, "</") {
					if pre > 0 {
						pre--
					}
				} else {
					pre++
				}
			}
		} else if comment {
			l.hidden = true
			if strings.Contains(trimmed, "-->") {
				comment = false
			}
		} else if strings.Contains(trimmed, "<!--") && !strings.HasPrefix(trimmed, "<!-- cr-comment") && !publicationMarker.MatchString(trimmed) {
			l.hidden = true
			comment = !strings.Contains(trimmed[strings.Index(trimmed, "<!--")+4:], "-->")
		} else if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				fence, fenceLength, fenceQuote = trimmed[0], n, quote
				l.hidden = true
			}
		}
		lines = append(lines, l)
		offset = next
	}
	return lines, fence == 0 && pre == 0 && !comment
}

type findingDecoder struct {
	inventory *HostedFindingInventory
	seen      map[string]bool
}

func (d *findingDecoder) fail(reason string) {
	d.inventory.Complete = false
	if d.inventory.Reason == "" {
		d.inventory.Reason = reason
	}
}

func (d *findingDecoder) add(body string, start int, line findingLine, f HostedFinding) {
	match := findingMarker.FindStringSubmatch(line.text)
	if match == nil {
		d.fail("finding_fingerprint_invalid")
		return
	}
	f.Fingerprint = match[1]
	if d.seen[f.Fingerprint] {
		d.fail("finding_fingerprint_repeated")
		return
	}
	d.seen[f.Fingerprint] = true
	if f.Kind == "inline" {
		f.SourceKey = fmt.Sprintf("hosted-comment:%d:%s", f.CommentID, f.Fingerprint)
	} else {
		f.SourceKey = fmt.Sprintf("hosted-review:%d:%s:%s", f.ReviewID, f.Kind, f.Fingerprint)
	}
	if f.Title == "" {
		f.Title = f.SourceKey
	}
	if len(f.Title) > 500 {
		d.fail("finding_title_limit")
		return
	}
	if line.end-start > 16<<10 {
		d.fail("finding_detail_limit")
		return
	}
	if len(d.inventory.Findings) >= 100 {
		d.fail("finding_inventory_limit")
		return
	}
	f.Detail = body[start:line.end]
	if f.Severity == "" {
		f.Severity = "unknown"
	}
	f.StartLine = copyPublicationLine(f.StartLine)
	f.EndLine = copyPublicationLine(f.EndLine)
	d.inventory.Findings = append(d.inventory.Findings, f)
}

var findingPictureSeverity = regexp.MustCompile(`^` + standingPicture + ` (Trivial|Minor|Major|Critical)$`)

func findingSeverity(text string) string {
	text = strings.TrimSpace(text)
	if match := findingPictureSeverity.FindStringSubmatch(text); match != nil {
		text = match[1]
	}
	for _, prefix := range []string{"🔵 ", "🟡 ", "🟠 ", "🔴 "} {
		text = strings.TrimPrefix(text, prefix)
	}
	switch text {
	case "Trivial":
		return "info"
	case "Minor":
		return "low"
	case "Major":
		return "high"
	case "Critical":
		return "critical"
	}
	return ""
}

type findingFields struct {
	title, severity, path string
	start, end            *int64
	severityConflict      bool
}

func (f *findingFields) severityValue(value string) {
	if value == "" {
		return
	}
	if f.severity != "" && f.severity != value {
		f.severityConflict = true
	}
	f.severity = value
}

func (f *findingFields) metadata(text string) (valid, explicitSeverity bool) {
	separator := " | "
	if strings.Contains(text, " · ") {
		if strings.Contains(text, separator) {
			return false, false
		}
		separator = " · "
	}
	parts := strings.Split(text, separator)
	for i, p := range parts {
		switch {
		case len(p) >= 4 && strings.HasPrefix(p, "**") && strings.HasSuffix(p, "**"):
			parts[i] = p[2 : len(p)-2]
		case len(p) >= 2 && strings.HasPrefix(p, "_") && strings.HasSuffix(p, "_"):
			parts[i] = p[1 : len(p)-1]
		default:
			return false, false
		}
	}
	for _, p := range parts {
		value := findingSeverity(p)
		f.severityValue(value)
		explicitSeverity = explicitSeverity || value != ""
	}
	return true, explicitSeverity
}

func (f *findingFields) observe(text string) {
	if m := findingTitle.FindStringSubmatch(text); m != nil && f.title == "" {
		f.title = m[1]
	}
	f.metadata(text)
}

func (f findingFields) apply(result *HostedFinding) {
	result.Title, result.Severity = f.title, f.severity
	if f.severityConflict {
		result.Severity = "unknown"
	}
}

func findingLocation(first, last string) (*int64, *int64, bool) {
	a, e := strconv.ParseInt(first, 10, 64)
	if e != nil || a < 1 || a > 2147483647 {
		return nil, nil, false
	}
	if last == "" {
		last = first
	}
	b, e := strconv.ParseInt(last, 10, 64)
	if e != nil || b < a || b > 2147483647 {
		return nil, nil, false
	}
	return &a, &b, true
}

func findingSourceMetadata(text string) bool {
	return text == "" || (strings.HasPrefix(text, "_Source: ") || strings.HasPrefix(text, "_Sources: ")) && strings.HasSuffix(text, "_")
}

// An edited provider acknowledgement is metadata only. Require the complete
// known wrapper and bounded handle; neither its actor nor its claim creates a
// local disposition, fix proof or another source finding.
func findingConfirmationEnd(lines []findingLine, start int) (int, bool) {
	if lines[start].quote != 0 || lines[start].text != "<!-- This is an auto-generated reply by CodeRabbit -->" {
		return start, false
	}
	i := start + 1
	for i < len(lines) && lines[i].text == "" {
		i++
	}
	if i >= len(lines) || lines[i].quote != 0 || lines[i].hidden || !findingConfirmation.MatchString(lines[i].text) {
		return start, false
	}
	i++
	for i < len(lines) && lines[i].text == "" {
		i++
	}
	if i >= len(lines) || lines[i].quote != 0 || lines[i].text != "<!-- This is an auto-generated comment by CodeRabbit -->" {
		return start, false
	}
	return i, true
}

func findingTags(text string, details, quotes *int) {
	if !strings.HasPrefix(text, "<") {
		return
	}
	for _, tag := range findingHTML.FindAllString(text, -1) {
		depth := details
		if strings.Contains(tag, "blockquote") {
			depth = quotes
		}
		if strings.HasPrefix(tag, "</") {
			*depth--
		} else {
			*depth++
		}
	}
}

// findingBlock identifies the matching explicit details scope. A same-level
// marker in an unknown scope remains unsupported, never silently discarded.
func findingBlock(lines []findingLine, start, quote int) (int, bool) {
	depth := 0
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if l.hidden || l.text == "" {
			continue
		}
		if l.quote > quote {
			continue
		}
		if l.quote != quote {
			return i, false
		}
		unused := 0
		findingTags(l.text, &depth, &unused)
		if depth == 0 {
			return i, true
		}
		if depth < 0 {
			return i, false
		}
	}
	return len(lines) - 1, false
}

func findingKnownNested(text string) bool {
	return text == "<summary>🤖 Prompt for AI Agents</summary>" || text == "<summary>Proposed wording</summary>" || text == "<summary>Suggested change</summary>" || text == "<summary>Example</summary>"
}

func findingNestedHeader(lines []findingLine, start, end int) string {
	if position := strings.Index(lines[start].text, "<summary>"); position >= 0 {
		return lines[start].text[position:]
	}
	return nextFindingText(lines, start+1, end)
}

// The provider may group several separately fingerprinted findings in one
// GitHub comment. Preserve the ordinary root-count contract; a larger declared
// count is proven only by the complete existing inline decoder, never by raw
// marker occurrences, prompt examples or summary findings.
func hostedInlineCountMatches(roots []ReviewComment, declared int) bool {
	if len(roots) == declared {
		return true
	}
	if len(roots) == 0 || len(roots) > declared {
		return false
	}
	inventory := HostedFindingInventory{Complete: true, Findings: []HostedFinding{}}
	decoder := findingDecoder{inventory: &inventory, seen: map[string]bool{}}
	for _, root := range roots {
		decoder.inline(root)
	}
	return inventory.Complete && len(inventory.Findings) == declared
}

func (d *findingDecoder) inline(root ReviewComment) {
	lines, sound := findingLines(root.Body)
	if !sound {
		d.fail("finding_format_unsupported")
	}
	fields := findingFields{}
	start := 0
	markers := 0
	after := false
	pending := false
	blockquotes := 0
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if after && !pending {
			if end, ok := findingConfirmationEnd(lines, i); ok {
				i = end
				start = lines[end].next
				continue
			}
		}
		if l.hidden || l.quote != 0 {
			continue
		}
		if findingQuotedHTML.MatchString(l.text) || blockquotes > 0 {
			for _, tag := range findingQuotedHTML.FindAllString(l.text, -1) {
				if strings.HasPrefix(tag, "</") {
					blockquotes--
				} else {
					blockquotes++
				}
			}
			if blockquotes < 0 {
				d.fail("finding_format_unsupported")
			}
			continue
		}
		if strings.HasPrefix(l.text, "<details>") {
			end, ok := findingBlock(lines, i, 0)
			if !ok {
				d.fail("finding_format_unsupported")
				break
			}
			// Nested prompts are retained as raw detail, but their examples do not
			// supply source markers or other authoritative fields.
			for j := i + 1; j < end; j++ {
				if !lines[j].hidden && lines[j].quote == 0 && strings.Contains(lines[j].text, "cr-comment") && !findingKnownNested(findingNestedHeader(lines, i, end)) {
					d.fail("finding_format_unsupported")
				}
			}
			if after && !pending {
				start = l.start
				pending = true
			}
			i = end
			continue
		}
		if strings.HasPrefix(strings.ToLower(l.text), "<details") {
			d.fail("finding_format_unsupported")
		}
		if strings.Contains(l.text, "cr-comment") {
			f := HostedFinding{Kind: "inline", ReviewID: root.ReviewID, CommentID: root.ID, Path: root.Path, Side: root.Side}
			if root.OriginalStartLine != nil || root.OriginalLine != nil {
				f.StartLine, f.EndLine = root.OriginalStartLine, root.OriginalLine
			} else {
				f.StartLine, f.EndLine = root.StartLine, root.Line
			}
			fields.apply(&f)
			d.add(root.Body, start, l, f)
			markers++
			fields = findingFields{}
			after = true
			pending = false
			start = l.next
			continue
		}
		if after && !pending {
			if findingSourceMetadata(l.text) {
				start = l.next
				continue
			}
			start = l.start
			pending = true
		}
		fields.observe(l.text)
	}
	if blockquotes != 0 {
		d.fail("finding_format_unsupported")
	}
	if markers == 0 {
		d.fail("finding_marker_missing")
	} else if pending {
		d.fail("finding_trailer_unsupported")
	}
}

func nextFindingText(lines []findingLine, start, end int) string {
	for i := start; i < end; i++ {
		if !lines[i].hidden && lines[i].text != "" {
			return lines[i].text
		}
	}
	return ""
}

func findingCount(text string) (int, bool) {
	n, err := strconv.ParseInt(text, 10, 32)
	return int(n), err == nil && n >= 0 && n <= 100
}

// outsideItem owns one quoted details/blockquote item. Nested prompts remain in
// Detail but cannot create extra atomic findings or replace the full title/path.
func (d *findingDecoder) outsideItem(body string, lines []findingLine, start, end int, reviewID int64) int {
	fields := findingFields{}
	depth, quotes := 0, 0
	markers := 0
	closed := false
	knownNested := false
	summary := false
	for i := start; i <= end; i++ {
		l := lines[i]
		if l.hidden || l.quote != 1 {
			continue
		}
		if depth == 1 && quotes == 0 && findingOutsideSummary.MatchString(l.text) {
			summary = true
			match := findingOutsideSummary.FindStringSubmatch(l.text)
			fields.severityValue(findingSeverity(match[1] + match[2]))
		}
		if depth > 1 && strings.HasPrefix(l.text, "<summary>") {
			knownNested = findingKnownNested(l.text)
		}
		if strings.Contains(l.text, "cr-comment") {
			if depth == 1 && quotes == 1 {
				if closed {
					d.fail("finding_format_unsupported")
				}
				if !summary || fields.path == "" || fields.title == "" {
					d.fail("finding_format_unsupported")
				}
				f := HostedFinding{Kind: "outside_diff", ReviewID: reviewID, Path: fields.path, StartLine: fields.start, EndLine: fields.end}
				fields.apply(&f)
				d.add(body, lines[start].start, l, f)
				markers++
				closed = true
			} else if !knownNested {
				d.fail("finding_format_unsupported")
			}
		}
		if depth == 1 && quotes == 1 && !strings.HasPrefix(l.text, "<") {
			if closed && !findingSourceMetadata(l.text) {
				d.fail("finding_trailer_unsupported")
			}
			if !closed {
				if m := findingPathLocation.FindStringSubmatch(l.text); m != nil {
					a, b, ok := findingLocation(m[2], m[3])
					if !ok || !validRepoPath(m[1]) || fields.path != "" {
						d.fail("finding_location_invalid")
					} else {
						fields.path, fields.start, fields.end = m[1], a, b
					}
				}
				fields.observe(l.text)
			}
		}
		if closed && depth == 1 && l.text == "<details>" {
			d.fail("finding_trailer_unsupported")
		}
		findingTags(l.text, &depth, &quotes)
		if depth <= 1 {
			knownNested = false
		}
		if depth < 0 || quotes < 0 {
			d.fail("finding_format_unsupported")
		}
	}
	if !summary || markers != 1 || depth != 0 || quotes != 0 {
		d.fail("finding_format_unsupported")
	}
	return markers
}

func (d *findingDecoder) outside(body string, lines []findingLine, header int, reviewID int64) int {
	m := findingOutsideHeader.FindStringSubmatch(lines[header].text)
	expected, ok := findingCount(m[1])
	if !ok {
		d.fail("finding_inventory_limit")
	}
	count := 0
	i := header + 1
	for ; i < len(lines); i++ {
		l := lines[i]
		if l.hidden || l.text == "" {
			continue
		}
		if l.quote != 1 {
			break
		}
		if l.text != "<details>" {
			d.fail("summary_format_unsupported")
			continue
		}
		end, sound := findingBlock(lines, i, 1)
		if !sound {
			d.fail("summary_format_unsupported")
			return end
		}
		count += d.outsideItem(body, lines, i, end, reviewID)
		i = end
	}
	if count != expected {
		d.fail("finding_count_mismatch")
	}
	return i - 1
}

func (d *findingDecoder) duplicateGroup(body string, lines []findingLine, start, end int, reviewID int64) int {
	fields := findingFields{}
	depth, quotes := 0, 0
	count, expected := 0, -1
	path := ""
	itemStart := -1
	knownNested := false
	for i := start; i <= end; i++ {
		l := lines[i]
		if l.hidden || l.quote != 0 {
			continue
		}
		if depth == 1 && quotes == 0 {
			if m := findingGroupHeader.FindStringSubmatch(l.text); m != nil {
				path = m[1]
				var ok bool
				expected, ok = findingCount(m[2])
				if !ok || !validRepoPath(path) {
					d.fail("finding_location_invalid")
				}
				if !validRepoPath(path) {
					path = ""
				}
			}
		}
		if depth > 1 && strings.HasPrefix(l.text, "<summary>") {
			knownNested = findingKnownNested(l.text)
		}
		if depth == 1 && quotes == 1 {
			if m := findingRange.FindStringSubmatch(l.text); m != nil {
				if itemStart >= 0 {
					d.fail("finding_marker_missing")
				}
				a, b, ok := findingLocation(m[1], m[2])
				if !ok {
					d.fail("finding_location_invalid")
				}
				fields = findingFields{path: path, start: a, end: b}
				fields.metadata(m[3])
				itemStart = l.start
			} else if strings.Contains(l.text, "cr-comment") {
				if itemStart < 0 {
					d.fail("finding_format_unsupported")
				} else {
					if fields.title == "" || path == "" {
						d.fail("finding_format_unsupported")
					}
					f := HostedFinding{Kind: "duplicate", ReviewID: reviewID, Path: path, StartLine: fields.start, EndLine: fields.end}
					fields.apply(&f)
					d.add(body, itemStart, l, f)
					count++
					itemStart = -1
				}
			} else if itemStart >= 0 {
				fields.observe(l.text)
			} else if !findingSourceMetadata(l.text) && !strings.HasPrefix(l.text, "</") {
				d.fail("finding_trailer_unsupported")
			}
		} else if strings.Contains(l.text, "cr-comment") && !knownNested {
			d.fail("finding_format_unsupported")
		}
		findingTags(l.text, &depth, &quotes)
		if depth <= 1 {
			knownNested = false
		}
		if depth < 0 || quotes < 0 {
			d.fail("finding_format_unsupported")
		}
	}
	if expected < 0 || expected != count {
		d.fail("finding_count_mismatch")
	}
	if itemStart >= 0 {
		d.fail("finding_marker_missing")
	}
	if depth != 0 || quotes != 0 {
		d.fail("summary_format_unsupported")
	}
	return count
}

func (d *findingDecoder) duplicates(body string, lines []findingLine, start, end int, reviewID int64) {
	count, expected := 0, -1
	for i := start + 1; i < end; i++ {
		l := lines[i]
		if l.hidden || l.text == "" {
			continue
		}
		if m := findingDuplicateHeader.FindStringSubmatch(l.text); m != nil && expected < 0 {
			var ok bool
			expected, ok = findingCount(m[1])
			if !ok {
				d.fail("finding_inventory_limit")
			}
			continue
		}
		if l.text == "<details>" && l.quote == 0 {
			groupEnd, ok := findingBlock(lines, i, 0)
			if !ok || groupEnd >= end {
				d.fail("summary_format_unsupported")
				return
			}
			count += d.duplicateGroup(body, lines, i, groupEnd, reviewID)
			i = groupEnd
			continue
		}
		d.fail("summary_format_unsupported")
	}
	if expected < 0 || expected != count {
		d.fail("finding_count_mismatch")
	}
}

func (d *findingDecoder) summary(review HostedReview, selected bool) {
	if !selected && !strings.Contains(review.Body, "cr-comment") && !strings.Contains(review.Body, "Outside diff range comments") && !strings.Contains(review.Body, "Duplicate comments") {
		return
	}
	lines, sound := findingLines(review.Body)
	if !sound {
		d.fail("summary_format_unsupported")
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if l.hidden || l.text == "" {
			continue
		}
		if l.quote == 1 && findingOutsideHeader.MatchString(l.text) {
			i = d.outside(review.Body, lines, i, review.ID)
			continue
		}
		if l.quote == 0 && l.text == "<details>" {
			end, ok := findingBlock(lines, i, 0)
			if !ok {
				d.fail("summary_format_unsupported")
				return
			}
			header := nextFindingText(lines, i+1, end)
			if findingDuplicateHeader.MatchString(header) {
				d.duplicates(review.Body, lines, i, end, review.ID)
			} else {
				known := header == "<summary>🤖 Prompt to fix review comments</summary>" || findingPromptIcon.MatchString(header) || header == "<summary>ℹ️ Review info</summary>" || findingReviewInfoIcon.MatchString(header)
				if !known && selected {
					d.fail("summary_format_unsupported")
				}
				for j := i + 1; j < end; j++ {
					if !lines[j].hidden && strings.Contains(lines[j].text, "cr-comment") {
						d.fail("finding_format_unsupported")
					}
				}
			}
			i = end
			continue
		}
		if strings.Contains(l.text, "cr-comment") {
			d.fail("finding_format_unsupported")
			continue
		}
		if strings.Contains(l.text, "Outside diff range comments") || strings.Contains(l.text, "Duplicate comments") {
			d.fail("summary_format_unsupported")
			continue
		}
		if !selected {
			continue
		} // Markerless status/chatter remains retained, not a new batch.
		if publicationMarker.MatchString(l.text) || publicationActionable.MatchString(l.text) || l.text == "---" || l.quote == 1 && (l.text == "[!CAUTION]" || strings.HasPrefix(l.text, "Some comments are outside the diff")) || strings.HasPrefix(l.text, "- [ ] <!-- {\"checkboxId\":") {
			continue
		}
		d.fail("summary_format_unsupported")
	}
}

// publicationOutsideOnly recognizes the narrow omitted-inline-count variant
// through the same strict decoder used for final extraction. It is nonrecursive
// and does not rewrite the original provider body or infer an empty inventory.
func publicationOutsideOnly(publication HostedPublication) bool {
	if len(publication.Batches) != 1 {
		return false
	}
	primary := publication.Batches[0]
	inventory := HostedFindingInventory{Complete: true, Findings: []HostedFinding{}}
	decoder := findingDecoder{inventory: &inventory, seen: map[string]bool{}}
	decoder.summary(primary, true)
	if !inventory.Complete || len(inventory.Findings) == 0 {
		return false
	}
	for _, review := range publication.EligibleReviews {
		for _, line := range publicationVisibleLines(review.Body) {
			if strings.Contains(strings.ToLower(line), "actionable comments posted") {
				return false
			}
		}
		if review.ID != primary.ID {
			decoder.summary(review, false)
		}
	}
	if !inventory.Complete || len(inventory.Findings) == 0 {
		return false
	}
	for _, finding := range inventory.Findings {
		if finding.Kind != "outside_diff" {
			return false
		}
	}
	return true
}

// ExtractHostedFindings re-establishes attribution before decoding. Unsupported
// inventory remains explicitly incomplete with bounded extracted pieces and the
// original publication retained; it never fabricates aggregate placeholder rows.
func ExtractHostedFindings(snapshot HostedPublicationSnapshot, intent HostedTriggerIntent, postingActor, providerActor Author, earliestPosting time.Time) (HostedFindingInventory, error) {
	publication, err := ResolveHostedPublication(snapshot, intent, postingActor, providerActor, earliestPosting)
	result := HostedFindingInventory{Publication: publication, Findings: []HostedFinding{}}
	if err != nil {
		result.Reason = "publication_unavailable"
		return result, err
	}
	return decodeHostedFindings(publication), nil
}

// decodeHostedFindings shares only inventory decoding; callers establish source
// attribution independently. It performs no request/attempt or coverage action.
func decodeHostedFindings(publication HostedPublication) HostedFindingInventory {
	result := HostedFindingInventory{Publication: publication, Findings: []HostedFinding{}}
	if publication.State != "attributed" {
		result.Reason = publication.Reason
		return result
	}
	if publication.StandingZero != nil {
		result.Complete = validStandingZero(publication.StandingZero) && len(publication.Batches) == 0 && len(publication.Roots) == 0 && len(publication.EligibleReviews) == 0
		result.Reason = "standing_zero_inventory_attributed"
		return result
	}
	result.Complete = true
	decoder := findingDecoder{inventory: &result, seen: map[string]bool{}}
	for _, root := range publication.Roots {
		decoder.inline(root)
	}
	selected := map[int64]bool{}
	for _, batch := range publication.Batches {
		selected[batch.ID] = true
	}
	for _, review := range publication.EligibleReviews {
		decoder.summary(review, selected[review.ID])
	}
	if result.Complete {
		result.Reason = "finding_inventory_complete"
	}
	return result
}
