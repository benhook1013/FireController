package reviewprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// The Additional comments envelope uses explicit dl/dd presentation wrappers.
// Hide only balanced standalone wrappers at their original details/quote depth;
// source offsets and finding text remain verbatim for extraction.
func findingAdditionalLayout(lines []findingLine, start, end int) ([]findingLine, bool) {
	projected := append([]findingLine(nil), lines...)
	type frame struct {
		tag             string
		details, quotes int
	}
	var stack []frame
	details, quotes := 0, 0
	for i := start; i <= end; i++ {
		l := lines[i]
		if l.hidden || l.quote != 0 {
			continue
		}
		switch l.text {
		case "<dl>", "<dd>":
			if l.text == "<dd>" && (len(stack) == 0 || stack[len(stack)-1].tag != "<dl>" || stack[len(stack)-1].details != details || stack[len(stack)-1].quotes != quotes) {
				return nil, false
			}
			stack = append(stack, frame{l.text, details, quotes})
			projected[i].hidden = true
		case "</dd>", "</dl>":
			tag := "<dd>"
			if l.text == "</dl>" {
				tag = "<dl>"
			}
			if len(stack) == 0 || stack[len(stack)-1] != (frame{tag, details, quotes}) {
				return nil, false
			}
			stack = stack[:len(stack)-1]
			projected[i].hidden = true
		default:
			findingTags(l.text, &details, &quotes)
			if len(stack) > 0 && (details < stack[len(stack)-1].details || quotes < stack[len(stack)-1].quotes) {
				return nil, false
			}
		}
	}
	return projected, len(stack) == 0 && details == 0 && quotes == 0
}

// Additional items have no provider fingerprint marker. Their identity is
// derived from the attributed review, location and exact original detail span;
// it does not pretend to identify a provider inline comment or verified issue.
func (d *findingDecoder) additional(body string, start, end int, f HostedFinding) {
	if start < 0 || end < start || end > len(body) || f.StartLine == nil || f.EndLine == nil || *f.StartLine <= 0 || *f.EndLine < *f.StartLine || !validRepoPath(f.Path) || f.ReviewID <= 0 {
		d.fail("finding_format_unsupported")
		return
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("additional:%d:%s:%d:%d:%s", f.ReviewID, f.Path, *f.StartLine, *f.EndLine, body[start:end])))
	f.Fingerprint = hex.EncodeToString(digest[:12])
	d.record(body, start, end, f)
}
