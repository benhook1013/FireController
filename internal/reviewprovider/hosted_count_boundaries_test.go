package reviewprovider

import (
	"strings"
	"testing"
)

func TestSubmittedCountRenderingRespectsOrdinaryBoundaries(t *testing.T) {
	if !hasSubmittedSoftWrappedCount("Actionable comments posted\n2") {
		t.Fatal("ordinary label and numeric continuation were not recognized")
	}
	for _, text := range []string{"Release notes\n1. Read two documents.", "See the actionable comments posted\n* 2 examples appear below.", "The phrase __Actionable comments posted__ appears in this sentence."} {
		if hasSubmittedSoftWrappedCount(text) {
			t.Fatal("ordinary content was treated as a count declaration")
		}
	}
	for _, text := range []string{"1. Read two documents.", "* 2 examples appear below."} {
		if !submittedListInterrupt(findingLine{countText: text}) {
			t.Fatal("list item must interrupt the paragraph")
		}
	}
	var remaining strings.Builder
	if splitSubmittedPreCounts("<pre>Findings:</pre><pre>2 releases remain.</pre>", &remaining, func(text string) bool {
		return standingFindingClaim.MatchString(submittedCountWhitespace(text))
	}) {
		t.Fatal("separate code examples were joined into one count")
	}
}

func TestSubmittedCountProjectionNormalizesPunctuationSeparately(t *testing.T) {
	if !hasBareSubmittedActionableLabel("__Actionable comments posted__") {
		t.Fatal("formatted standalone label not recognized")
	}
	if got := normalizeSubmittedEscapedColon(`Example\: punctuation`); got != "Example: punctuation" {
		t.Fatalf("punctuation escape not rendered: %q", got)
	}
	if got := normalizeSubmittedEscapedColon(`Example\\: punctuation`); got != `Example\: punctuation` {
		t.Fatalf("literal backslash did not survive: %q", got)
	}
}
