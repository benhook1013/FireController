package reviewprovider

import (
	"strings"
	"testing"
)

func TestHostedPreLexerHonorsQuotedTagBoundaries(t *testing.T) {
	body := `<PRE class="example > caption">sample <span title="literal <pre> text">text</span></PRE><pre>next</pre>`
	tags := submittedPreTags(body)
	if len(tags) != 4 || tags[0][2] != 1 || tags[1][2] != -1 || tags[2][2] != 1 || tags[3][2] != -1 {
		t.Fatalf("wrong pre tag sequence: %v", tags)
	}
	var projected []string
	var count strings.Builder
	if splitSubmittedPreCounts(body, &count, func(s string) bool { projected = append(projected, strings.TrimSpace(s)); return false }) {
		t.Fatal("ordinary preformatted text cannot signal a count")
	}
	if len(projected) != 2 || projected[0] != "sample text" || projected[1] != "next" {
		t.Fatalf("incorrect preformatted fragments: %q", projected)
	}
}
func TestHostedPreLexerKeepsEditorialCommentInsideRawBlock(t *testing.T) {
	body := "<pre>Demo\n<!-- Editorial note\n    -->\nText\n</pre>\n\n    Example -->"
	if got := findingLastUnfencedCommentClose(body); got != strings.Index(body, "-->") {
		t.Fatalf("unexpected comment delimiter offset: %d", got)
	}
}
func TestHostedStandaloneCountLabelAndSummaryListOwnerGrammar(t *testing.T) {
	if !hasBareSubmittedActionableLabel("* **Actionable comments posted**") || hasBareSubmittedActionableLabel("We discuss **Actionable comments posted** here.") {
		t.Fatal("standalone list label and ordinary prose must be distinct")
	}
	owner := []string{"details", "dl", "dd", "p"}
	if !standingImplicitSummaryClose(owner, "dd") || !standingImplicitSummaryClose(owner, "dt") || standingImplicitSummaryClose(owner, "em") ||
		standingImplicitSummaryClose(append(owner, "dl", "dd"), "dd") {
		t.Fatal("description item ownership changed")
	}
}
