package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func TestHostedGroupedInlineInventoryMatchesDeclaredFindings(t *testing.T) {
	for _, kind := range []string{"publication", "submitted"} {
		t.Run(kind, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1) + "\n\n---\n\n" + hfInline(3), hfInline(2)}, "")
			if kind == "submitted" {
				f = submittedTestFixture(t)
				f.snapshot.ReviewComments[0].Body += "\n\n---\n\n" + hfInline(3)
			}
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Actionable comments posted: 2", "Actionable comments posted: 3", 1)
			before := append([]ReviewComment(nil), f.snapshot.ReviewComments...)
			got := f.extract(t)
			if !got.Complete || got.Publication.State != "attributed" || len(got.Publication.Roots) != 2 || len(got.Findings) != 3 || got.Findings[0].CommentID != got.Findings[1].CommentID || got.Findings[0].SourceKey == got.Findings[1].SourceKey {
				t.Fatal("grouped findings lost inventory or real identities", got)
			}
			if !reflect.DeepEqual(before, f.snapshot.ReviewComments) {
				t.Fatal("grouped count rewrote source")
			}
		})
	}
}

func TestHostedGroupedCountRequiresCompleteUniqueInlineInventory(t *testing.T) {
	for _, mode := range []string{"missing_finding", "duplicate_fingerprint", "quoted_marker", "fenced_marker", "nested_prompt_marker", "unsupported_trailer", "too_many_findings", "summary_cannot_fill_inline_count"} {
		t.Run(mode, func(t *testing.T) {
			first := hfInline(1) + "\n\n---\n\n" + hfInline(3)
			summary := ""
			switch mode {
			case "missing_finding":
				first = hfInline(1)
			case "duplicate_fingerprint":
				first = hfInline(1) + "\n\n" + hfInline(2)
			case "quoted_marker":
				first = hfInline(1) + "\n\n> " + hfMarker(3)
			case "fenced_marker":
				first = hfInline(1) + "\n\n```\n" + hfMarker(3) + "\n```"
			case "nested_prompt_marker":
				first = hfInline(1) + "\n\n<details>\n<summary>🤖 Prompt for AI Agents</summary>\n" + hfMarker(3) + "\n</details>"
			case "unsupported_trailer":
				first += "\n\nUnclassified extra finding."
			case "too_many_findings":
				first += "\n\n" + hfInline(4)
			case "summary_cannot_fill_inline_count":
				first = hfInline(1)
				summary = hfOutsideCategory(1, hfOutside(3, "engine/queue.go", "1-2", "Separate outside finding."))
			}
			f := hfNew(t, []string{first, hfInline(2)}, summary)
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Actionable comments posted: 2", "Actionable comments posted: 3", 1)
			got := f.extract(t)
			if got.Complete || got.Publication.State == "attributed" || len(got.Findings) != 0 {
				t.Fatal("unproven grouped inventory gained credit", mode, got)
			}
		})
	}
}
