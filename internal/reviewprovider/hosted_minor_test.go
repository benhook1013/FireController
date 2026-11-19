package reviewprovider

import (
	"fmt"
	"strings"
	"testing"
)

// Independently authored grammar fixture; no retained project data or prose.
const minorTestIcon = `<picture><img src="https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/amber-16.svg" width="16" height="16" align="absmiddle" alt=""></picture>`

func minorTestGroup(path string, count int, items ...string) string {
	return "<details>\n<summary>" + path + ": " + fmt.Sprint(count) + "</summary><blockquote>\n<dl>\n<dd>\n<br>\n\n" + strings.Join(items, "\n\n") + "\n\n</dd>\n</dl>\n</blockquote></details>"
}
func minorTestCategory(count int, groups ...string) string {
	return "<details>\n<summary>" + minorTestIcon + " Minor comments: " + fmt.Sprint(count) + "</summary><blockquote>\n<dl>\n<dd>\n<br>\n\n" + strings.Join(groups, "\n\n") + "\n\n</dd>\n</dl>\n</blockquote></details>"
}
func minorTestItem(n int) string {
	return "`24-26`: **" + minorTestIcon + " Minor** · **Functional Correctness** · **Low value**\n\n**Preserve the sample boundary.**\n\nIndependent synthetic rationale.\n\n<details>\n<summary>" + minorTestIcon + " Prompt for AI Agents</summary>\n```\nRepeated sample, not another finding.\n" + hfMarker(999) + "\n```\n</details>\n\n" + hfMarker(n)
}
func TestHostedMinorInventoryPreservesSource(t *testing.T) {
	item := minorTestItem(3)
	summary := hfOutsideCategory(1, hfOutside(2, "source/other.go", "12-15", "Preserve the record.")) + "\n\n" + minorTestCategory(1, minorTestGroup("source/sample.go", 1, item))
	got := hfWant(t, hfNew(t, []string{hfInline(1)}, summary), true, 3).Findings[2]
	if got.Kind != "minor" || got.Path != "source/sample.go" || got.ReviewID != 401 || got.CommentID != 0 || got.StartLine == nil || *got.StartLine != 24 || got.EndLine == nil || *got.EndLine != 26 || got.Fingerprint != fmt.Sprintf("%024x", 3) || got.SourceKey != "hosted-review:401:minor:"+fmt.Sprintf("%024x", 3) || got.Title != "Preserve the sample boundary." || !strings.Contains(got.Detail, "Independent synthetic rationale.") || got.Severity != "low" {
		t.Fatalf("lost source: %+v", got)
	}
}
func TestHostedMinorInventoryRejectsMalformed(t *testing.T) {
	item := minorTestItem(3)
	base := minorTestCategory(1, minorTestGroup("source/sample.go", 1, item))
	cases := map[string]string{
		"unknown nested":        strings.Replace(base, "Prompt for AI Agents", "Unknown nested group", 1),
		"missing severity":      strings.ReplaceAll(base, " Minor**", " Unspecified**"),
		"invalid metadata":      strings.Replace(base, "**Functional Correctness**", "Functional Correctness", 1),
		"file header text":      strings.Replace(base, "sample.go: 1", "sample.go: 1 extra", 1),
		"bad range":             strings.Replace(base, "`24-26`", "`26-24`", 1),
		"total mismatch":        strings.Replace(base, "Minor comments: 1", "Minor comments: 2", 1),
		"file mismatch":         strings.Replace(base, "sample.go: 1", "sample.go: 2", 1),
		"unknown group":         strings.Replace(base, "Minor comments:", "Other comments:", 1),
		"unknown file grammar":  strings.Replace(base, "sample.go: 1", "sample.go (1)", 1),
		"missing marker":        strings.Replace(base, hfMarker(3), "", 1),
		"malformed marker":      strings.Replace(base, hfMarker(3), "<!-- cr-comment:v1:bad -->", 1),
		"invalid path":          strings.Replace(base, "source/sample.go", "../sample.go", 1),
		"unbalanced layout":     strings.Replace(base, "</dd>\n</dl>", "</dl>\n</dd>", 1),
		"negative count":        strings.Replace(base, "Minor comments: 1", "Minor comments: -1", 1),
		"overflow":              strings.Replace(base, "Minor comments: 1", "Minor comments: 99999999999999999999999", 1),
		"duplicate fingerprint": minorTestCategory(2, minorTestGroup("source/sample.go", 2, item, item)),
		"zero with finding":     strings.Replace(base, "Minor comments: 1", "Minor comments: 0", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hfNew(t, nil, body).extract(t); got.Complete {
				t.Fatalf("credited malformed inventory: %+v", got)
			}
		})
	}
}
func TestHostedMinorMultipleGroups(t *testing.T) {
	hfWant(t, hfNew(t, nil, minorTestCategory(2, minorTestGroup("source/one.go", 1, minorTestItem(3)), minorTestGroup("source/two.go", 1, minorTestItem(4)))), true, 2)
}
