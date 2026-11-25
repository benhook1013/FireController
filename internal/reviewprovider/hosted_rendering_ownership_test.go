package reviewprovider

import (
	"strings"
	"testing"
)

func TestFindingPreformattedEditorialBoundaries(t *testing.T) {
	body := "<pre>Example</pre><!-- Editorial note\n" +
		"Hidden annotation.\n-->\nVisible text."
	lines, sound := findingLines(body)
	if !sound || len(lines) != 4 || !lines[1].hidden || lines[1].countText != "" ||
		lines[3].hidden || lines[3].countText != "Visible text." {
		t.Fatal("editorial comment should end before ordinary visible text")
	}
	body = "Outside -->\n\n```plain\nExample delimiter -->\n```"
	if got := findingLastUnfencedCommentClose(body); got != strings.Index(body, "-->") {
		t.Fatalf("separate code example contributed a delimiter: %d", got)
	}
}

func TestSubmittedProviderLabelIsSeparateFromProse(t *testing.T) {
	if !hasSubmittedLabeledActionableCount("Review notes: Actionable comments posted: 2") {
		t.Fatal("known provider label was not recognized")
	}
	if hasSubmittedLabeledActionableCount("The review notes refer to Actionable comments posted: 2 examples.") {
		t.Fatal("ordinary prose was treated as an explicit provider label")
	}
	text := `<div title="<p>">A sentence</div><p>Another sentence.</p>`
	edges := submittedCountParagraphEdges(text)
	if len(edges) != 2 || edges[0][0] != strings.LastIndex(text, "<p>") {
		t.Fatalf("paragraph edge was confused with an attribute literal: %v", edges)
	}
}
