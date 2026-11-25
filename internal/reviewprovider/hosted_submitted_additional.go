package reviewprovider

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The observed markerless additional-only submission has one counted category,
// followed immediately by the existing h6-wrapped Review info. This recognizes
// its envelope only; it never supplies an inferred actionable count or inventory.
func (p *submittedReviewParser) additionalOnly() bool {
	line, ok := p.next()
	if !ok || line.hidden || line.quote != 0 || line.text != "<details>" {
		return false
	}
	start := p.at
	end, complete := findingBlock(p.lines, start, 0)
	if !complete {
		return false
	}
	p.at++
	header, ok := p.text()
	match := findingAdditionalHeader.FindStringSubmatch(header)
	if !ok || match == nil {
		return false
	}
	count, err := strconv.Atoi(match[1])
	if err != nil || count < 1 || count > 200 {
		return false
	}
	p.at = end + 1
	return true
}

// A count declaration has a label separator (including malformed values), a
// numeric value without its separator, or a bare formatted label. Mentioning
// the phrase in ordinary finding prose is not a declaration.
var submittedActionableClaim = regexp.MustCompile(`(?i)\bactionable[ \t]+comments[ \t]+posted(?:[ \t]*:|[ \t]+[0-9]+\b)`)

// Only a nonempty ordered item starting at one can interrupt a CommonMark paragraph.
var submittedOrderedParagraphInterrupt = regexp.MustCompile(`^1[.)][ \t]+\S`)
var submittedBulletParagraphInterrupt = regexp.MustCompile(`^[-+*][ \t]+\S`)

func submittedListInterrupt(line findingLine) bool {
	return !line.hidden && (submittedOrderedParagraphInterrupt.MatchString(line.countText) ||
		submittedBulletParagraphInterrupt.MatchString(line.countText))
}

// Complete provider collections independently prove the absence of attached or
// in-window orphan roots before this check. The positive counted category must
// decode in full, and every additional finding must name a selected file. These
// are unverified additional findings, never a fabricated clean/zero result.
func submittedAdditionalInventory(review HostedReview, selected map[string]bool) bool {
	// The shared scanner excludes comments only outside code examples. Join
	// physical boundaries hidden by comments for count refusal, without changing
	// source offsets or promoting mixed comment lines to structural metadata.
	lines, sound := findingLines(review.Body)
	if !sound {
		return false
	}
	var count, visibleParagraph, preCount strings.Builder
	for _, line := range lines {
		if splitSubmittedPreCounts(line.preCountText, &preCount, hasSubmittedPlainActionableClaim) {
			return false
		}
		if preCount.Len() != 0 && !line.preCommentContinues {
			preCount.WriteByte(' ')
		}
		if !line.preCountContinues {
			if hasSubmittedPlainActionableClaim(submittedRenderedCountText(preCount.String())) {
				return false
			}
			preCount.Reset()
		}
		// A genuinely blank visible line ends a Markdown paragraph. A
		// comment-hidden boundary does not, and cannot evade the refusal check.
		listInterrupt := submittedListInterrupt(line)
		if line.countBreak || listInterrupt || line.text == "" && !line.hidden && !line.countContinues {
			if hasSubmittedActionableClaim(visibleParagraph.String()) || hasBareSubmittedActionableLabel(visibleParagraph.String()) {
				return false
			}
			visibleParagraph.Reset()
			if line.countBreak {
				count.Reset()
				continue
			}
		}
		text, previousEdge := line.countText, 0
		for _, edge := range submittedCountParagraphEdges(text) {
			count.WriteString(text[previousEdge:edge[0]])
			visibleParagraph.WriteString(text[previousEdge:edge[0]])
			if hasSubmittedActionableClaim(count.String()) || hasBareSubmittedActionableLabel(count.String()) ||
				hasSubmittedActionableClaim(visibleParagraph.String()) || hasBareSubmittedActionableLabel(visibleParagraph.String()) {
				return false
			}
			count.Reset()
			visibleParagraph.Reset()
			previousEdge = edge[1]
		}
		count.WriteString(text[previousEdge:])
		visibleParagraph.WriteString(text[previousEdge:])
		if line.countContinues {
			continue
		}
		if hasSubmittedActionableClaim(count.String()) || hasBareSubmittedActionableLabel(count.String()) {
			return false
		}
		count.Reset()
		visibleParagraph.WriteByte(' ')
	}
	// Ordinary Markdown soft breaks are whitespace within a paragraph.
	// Keep the per-line refusal above for bare labels as well.
	if hasSubmittedActionableClaim(visibleParagraph.String()) || hasBareSubmittedActionableLabel(visibleParagraph.String()) {
		return false
	}
	inventory := HostedFindingInventory{Complete: true, Findings: []HostedFinding{}}
	decoder := findingDecoder{inventory: &inventory, seen: map[string]bool{}}
	decoder.summary(review, true)
	if !inventory.Complete || len(inventory.Findings) == 0 {
		return false
	}
	for _, finding := range inventory.Findings {
		if finding.Kind != "additional" || !selected[finding.Path] {
			return false
		}
	}
	return true
}

// Attribute quotes can contain > without closing the tag. This is only a
// conservative count-refusal projection, not a general HTML inventory parser.
var submittedCountDecoration = regexp.MustCompile(`(?s)</?[A-Za-z](?:[^<>"']|"[^"]*"|'[^']*')*>`)

// A formatted label without a value belongs to the line-level grammar only.
// The same formatted phrase inside a normal sentence is not metadata.
var bareSubmittedActionableLabel = regexp.MustCompile(`(?i)^\s*(?:(?:[-+*]|#{1,6})\s+)?(?:\*\*\s*actionable\s+comments\s+posted\s*\*\*|__\s*actionable\s+comments\s+posted\s*__)\s*$`)

func hasBareSubmittedActionableLabel(line string) bool {
	return bareSubmittedActionableLabel.MatchString(submittedRenderedCountText(line))
}

// HTML layout breaks separate words, while inline style tags do not. This
// projection can refuse count metadata but cannot supply structure or offsets.
// The tag spans are quote-aware; an angle bracket inside an attribute is
// neither a pre boundary nor a separate token.
func submittedPreTags(text string) [][3]int {
	var tags [][3]int
	for _, span := range submittedCountDecoration.FindAllStringIndex(text, -1) {
		tag := text[span[0]:span[1]]
		name := submittedCountTagName.FindStringSubmatch(tag)
		if name == nil || !strings.EqualFold(name[1], "pre") {
			continue
		}
		change := 1
		if strings.HasPrefix(tag, "</") {
			change = -1
		}
		tags = append(tags, [3]int{span[0], span[1], change})
	}
	return tags
}

// A closing pre tag finishes the current count fragment even if another block
// begins on the same source line. This rendering never changes retained text.
func splitSubmittedPreCounts(text string, count *strings.Builder, check func(string) bool) bool {
	begin := 0
	for _, tag := range submittedPreTags(text) {
		if tag[2] > 0 {
			continue
		}
		count.WriteString(text[begin:tag[0]])
		if check(submittedRenderedCountText(count.String())) {
			return true
		}
		count.Reset()
		begin = tag[1]
	}
	count.WriteString(text[begin:])
	return false
}

func submittedCountParagraphEdges(text string) [][]int {
	var edges [][]int
	for _, span := range submittedCountDecoration.FindAllStringIndex(text, -1) {
		name := submittedCountTagName.FindStringSubmatch(text[span[0]:span[1]])
		if name != nil && strings.EqualFold(name[1], "p") {
			edges = append(edges, span)
		}
	}
	return edges
}

var submittedCountTagName = regexp.MustCompile(`^</?([A-Za-z][A-Za-z0-9-]*)`)

func submittedRenderedCountText(text string) string {
	return html.UnescapeString(submittedCountDecoration.ReplaceAllStringFunc(text, func(tag string) string {
		match := submittedCountTagName.FindStringSubmatch(tag)
		if match != nil {
			switch strings.ToLower(match[1]) {
			case "br", "p", "div", "li", "ul", "ol", "hr", "section", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "pre", "tr", "td", "th":
				return " "
			}
		}
		return ""
	}))
}

// Decoration removal is a refusal-only view: it cannot supply structural markers,
// inventory or source offsets. Both joining and separating tags are conservative
// refusal views: inline tags can split words, while br/block tags separate them.
// Keep the raw check as well so decorated examples
// cannot erase an already visible contradictory declaration.
// Inline Markdown link destinations do not contribute rendered count text.
// This refusal-only projection never yields structure or finding offsets.
var submittedCountMarkdownLink = regexp.MustCompile(`\[([^\]\r\n]+)\]\((?:[^()\r\n]|\([^()\r\n]*\))*\)`)

// Reference-link labels are a second, refusal-only rendering. Conservatively
// also examine their label alone when a reference is unresolved; this cannot
// supply trusted structure or proof of any inventory.
var submittedCountMarkdownReference = regexp.MustCompile(`\[([^\]\r\n]+)\]\[[^\]\r\n]*\]`)

func submittedCountWhitespace(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, text)
}

func hasSubmittedPlainActionableClaim(text string) bool {
	return submittedActionableClaim.MatchString(submittedCountWhitespace(text))
}

var submittedActionableLine = regexp.MustCompile(`(?i)^ *(?:(?:[-+]|[0-9]+[.)]|#{1,6}) +)?actionable +comments +posted(?: *:| +[0-9]+\b)`)

func hasSubmittedLineActionableClaim(text string) bool {
	return hasSubmittedActionableProjection(text, func(visible string) bool {
		return submittedActionableLine.MatchString(submittedCountWhitespace(visible))
	})
}

// The provider's explicit review-notes heading is a label, not ordinary
// prose. Refuse a count declaration immediately after that exact heading.
var submittedLabeledActionableCount = regexp.MustCompile(`(?i)^ *review +notes *: *actionable +comments +posted(?: *:| +[0-9]+\b)`)

func hasSubmittedLabeledActionableCount(text string) bool {
	return hasSubmittedActionableProjection(text, func(visible string) bool {
		return submittedLabeledActionableCount.MatchString(submittedCountWhitespace(visible))
	})
}

func hasSubmittedActionableClaim(text string) bool {
	return hasSubmittedActionableProjection(text, hasSubmittedPlainActionableClaim)
}

// Unescape only a punctuation escape consumed by Markdown. A pair of
// backslashes is one literal backslash, not a second escape introducer.
func normalizeSubmittedEscapedColon(text string) string {
	var result strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			if text[i+1] == ':' || text[i+1] == '\\' {
				i++
			}
		}
		result.WriteByte(text[i])
	}
	return result.String()
}

func hasSubmittedActionableProjection(text string, matches func(string) bool) bool {
	for _, visible := range []string{text,
		html.UnescapeString(submittedCountDecoration.ReplaceAllString(text, "")),
		html.UnescapeString(submittedCountDecoration.ReplaceAllString(text, " ")),
		submittedRenderedCountText(text),
	} {
		visible = submittedCountMarkdownLink.ReplaceAllString(visible, "${1}")
		for _, projected := range []string{visible, submittedCountMarkdownReference.ReplaceAllString(visible, "${1}")} {
			projected = normalizeSubmittedEscapedColon(projected)
			if matches(projected) || matches(
				strings.NewReplacer("*", "", "_", "", "~", "", "`", "").Replace(projected)) {
				return true
			}
		}
	}
	return false
}

// The omission fallback must distinguish an omitted count from a standalone
// label continued by a numeric line. Do not join prose or list items.
func hasSubmittedSoftWrappedCount(body string) bool {
	lines, sound := findingLines(body)
	if !sound {
		return true
	}
	label := false
	for _, line := range lines {
		if line.hidden || line.countBreak || line.countContinues || submittedListInterrupt(line) {
			label = false
			continue
		}
		visible := strings.TrimSpace(submittedRenderedCountText(line.countText))
		if label && hasSubmittedLineActionableClaim("Actionable comments posted "+visible) {
			return true
		}
		label = strings.EqualFold(visible, "Actionable comments posted")
	}
	return false
}
