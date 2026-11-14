package reviewprovider

import (
	"regexp"
	"strconv"
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
var submittedActionableClaim = regexp.MustCompile(`(?i)actionable comments posted(?:[ \t]*:|[ \t]+[0-9]+\b|[ \t]*(?:\*\*|$))`)

// Complete provider collections independently prove the absence of attached or
// in-window orphan roots before this check. The positive counted category must
// decode in full, and every additional finding must name a selected file. These
// are unverified additional findings, never a fabricated clean/zero result.
func submittedAdditionalInventory(review HostedReview, selected map[string]bool) bool {
	// Remove complete comments as a stream for count refusal only. Physical
	// newlines inside a comment must not split a visible count declaration.
	// Structural parsing below still uses the untouched original body.
	comment := false
	visible := findingCommentVisible(review.Body, &comment)
	lines, sound := findingLines(visible)
	if !sound || comment {
		return false
	}
	// Category bodies are HTML blockquotes, not quoted examples. Do not let
	// their visible count claims disappear behind publication metadata scopes.
	for _, line := range lines {
		if submittedActionableClaim.MatchString(line.countText) {
			return false
		}
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
