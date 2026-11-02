package reviewprovider

import "strings"

// HostedDisplaySeverity derives presentation metadata from a retained finding's
// bounded prefix. It does not validate publication, create credit, or change the
// stored severity. Only explicit leading metadata is eligible; narrative and
// nested examples cannot establish a severity. Oversized input is unknown.
func HostedDisplaySeverity(detail string) string {
	if len(detail) == 0 || len(detail) > 4096 {
		return "unknown"
	}
	lines, _ := findingLines(detail)
	fields := findingFields{}
	quote := 0
	started := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line.hidden || line.text == "" {
			continue
		}
		if !started && line.quote == 1 {
			// A quoted outside-diff finding has one exact owning summary.
			// Ordinary quoted metadata remains an example, not authority.
			if line.text != "<details>" {
				return "unknown"
			}
			j := i + 1
			for j < len(lines) && (lines[j].hidden || lines[j].text == "") {
				j++
			}
			if j == len(lines) || lines[j].quote != 1 {
				return "unknown"
			}
			match := findingOutsideSummary.FindStringSubmatch(lines[j].text)
			if match == nil {
				return "unknown"
			}
			fields.severityValue(findingSeverity(match[1] + match[2]))
			quote, started, i = 1, true, j
			continue
		}
		if line.quote != quote {
			break
		}
		if strings.HasPrefix(line.text, "<details>") {
			// Reuse the decoder's fence/quote-aware block boundary. Contents
			// of prompts or examples never supply presentation metadata.
			end, complete := findingBlock(lines, i, quote)
			if !complete {
				// A bounded prefix may end inside diagnostic detail. It cannot
				// erase explicit metadata already established before that block.
				break
			}
			i = end
			continue
		}
		if quote == 1 && findingPathLocation.MatchString(line.text) {
			continue
		}
		valid, explicit := fields.metadata(line.text)
		if !valid || findingTitle.MatchString(line.text) && !explicit {
			break
		}
		started = true
	}
	if fields.severity == "" || fields.severityConflict {
		return "unknown"
	}
	return fields.severity
}
