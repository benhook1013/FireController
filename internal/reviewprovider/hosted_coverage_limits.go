package reviewprovider

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var hostedUnavailableHeader = regexp.MustCompile(`^<details>\s*<summary>(?:` + standingPicture + ` )?Files not fully reviewed because their changes were unavailable: ([^<>]*)</summary>`)
var hostedProcessingFailureHeader = regexp.MustCompile(`^<details>\s*<summary>Files not reviewed due to moderation or processing errors: ([1-9][0-9]{0,4})</summary>`)
var hostedProcessingFailureList = regexp.MustCompile(`(?s)^<dl>\s*<dd>\s*<br>\s*(.*?)\s*</dd>\s*</dl>\s*</details>$`)
var hostedUnavailableList = regexp.MustCompile(`(?s)^<dl>\s*<dd>\s*(.*?)\s*</dd>\s*</dl>\s*</details>$`)

const hostedCoverageUnknown = "CodeRabbit reported unavailable file changes, but its affected-file metadata could not be verified."
const hostedCoverageMore = "Additional CodeRabbit coverage limitations were reported; see retained review metadata."

// Only direct generated children of a recognized Review info block supply
// coverage metadata. Other details scopes (including prompts/examples) and
// non-presentation HTML ancestors are opaque. Source bodies remain unchanged.
func (d *findingDecoder) reviewInfoCoverage(body string, lines []findingLine, start, end int) {
	forReviewInfoBlock(body, lines, start, end, func(block string) {
		if limit := hostedUnavailableCoverage(block); limit != "" {
			d.addCoverageLimit(limit)
		}
	})
}

func forReviewInfoBlock(body string, lines []findingLine, start, end int, visit func(string)) {
	var stack []string
	for i := start + 1; i < end; i++ {
		line := lines[i]
		if line.hidden || line.quote != 0 || line.text == "" {
			continue
		}
		if strings.HasPrefix(line.text, "<details>") {
			last, ok := findingBlock(lines, i, 0)
			if !ok || last >= end {
				return
			}
			presentation := true
			for _, tag := range stack {
				presentation = presentation && (tag == "dl" || tag == "dd")
			}
			if presentation {
				visit(strings.TrimSpace(body[line.start:lines[last].end]))
			}
			i = last
			continue
		}
		for _, token := range hostedComparisonTokens.FindAllString(line.text, -1) {
			tag := hostedComparisonTag.FindStringSubmatch(token)
			if tag == nil {
				continue
			}
			name := strings.ToLower(tag[1])
			if hostedComparisonVoid(name) {
				continue
			}
			if strings.HasPrefix(token, "</") {
				if len(stack) == 0 || stack[len(stack)-1] != name {
					return
				}
				stack = stack[:len(stack)-1]
			} else if !strings.HasSuffix(token, "/>") {
				stack = append(stack, name)
			}
		}
	}
}

// A current generated Review info block can attest to failed processing
// without proving any finding count or completed coverage.
func hostedReportedProcessingFailure(body string) bool {
	lines, valid := findingLines(body)
	if !valid {
		return false
	}
	for i, line := range lines {
		if line.hidden || line.quote != 0 || line.text != "<h6>" {
			continue
		}
		start, end, ok := currentReviewInfoHeading(lines, i)
		if !ok {
			continue
		}
		found := false
		forReviewInfoBlock(body, lines, start, end-1, func(block string) {
			header := hostedProcessingFailureHeader.FindStringSubmatchIndex(block)
			if header == nil {
				return
			}
			list := hostedProcessingFailureList.FindStringSubmatch(strings.TrimSpace(block[header[1]:]))
			if list == nil {
				return
			}
			count, _ := strconv.Atoi(block[header[2]:header[3]])
			paths := map[string]bool{}
			for _, item := range strings.Split(strings.TrimSpace(list[1]), "\n") {
				path, ok := strings.CutPrefix(strings.TrimSuffix(item, "\r"), "* ")
				if !ok || !cliPortablePath(path, false) || strings.ContainsAny(path, "<>`&") || paths[path] {
					return
				}
				paths[path] = true
			}
			found = found || count == len(paths)
		})
		if found {
			return true
		}
	}
	return false
}

func hostedUnavailableCoverage(body string) string {
	header := hostedUnavailableHeader.FindStringSubmatchIndex(body)
	if header == nil {
		return ""
	}
	countText := body[header[2]:header[3]]
	count, err := strconv.Atoi(countText)
	if err != nil || count < 0 || count > 10000 || strconv.Itoa(count) != countText {
		return hostedCoverageUnknown
	}
	list := hostedUnavailableList.FindStringSubmatch(strings.TrimSpace(body[header[1]:]))
	if list == nil {
		return hostedCoverageUnknown
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, text := range strings.Split(list[1], "\n") {
		text = strings.TrimSpace(strings.TrimSuffix(text, "\r"))
		if text == "" {
			continue
		}
		if !strings.HasPrefix(text, "* ") {
			return hostedCoverageUnknown
		}
		path := strings.TrimPrefix(text, "* ")
		if strings.TrimSpace(path) != path || !cliPortablePath(path, false) || strings.ContainsAny(path, "<>`&") || seen[path] {
			return hostedCoverageUnknown
		}
		seen[path] = true
		paths = append(paths, path)
	}
	if count != len(paths) {
		return hostedCoverageUnknown
	}
	if count == 0 {
		return ""
	}
	noun, possessive := "files", "their"
	if count == 1 {
		noun, possessive = "file", "its"
	}
	prefix := fmt.Sprintf("CodeRabbit reported %d %s not fully reviewed because %s changes were unavailable", count, noun, possessive)
	limit := prefix + ": " + strings.Join(paths, "; ") + "."
	if len(limit) > 1024 {
		return prefix + ". See retained review metadata for file paths."
	}
	return limit
}

func (d *findingDecoder) addCoverageLimit(limit string) {
	for _, old := range d.inventory.CoverageLimits {
		if old == limit {
			return
		}
	}
	if len(d.inventory.CoverageLimits) < 16 {
		d.inventory.CoverageLimits = append(d.inventory.CoverageLimits, limit)
	} else {
		d.inventory.CoverageLimits[15] = hostedCoverageMore
	}
}
