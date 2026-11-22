package reviewprovider

import "testing"

func TestHostedCountClaimsKeepWordBoundary(t *testing.T) {
	for _, phrase := range []string{
		"Nonactionable comments posted: 1",
		"Preserve the **actionable comments posted** label.",
	} {
		if hasSubmittedActionableClaim(phrase) || hasBareSubmittedActionableLabel(phrase) {
			t.Fatalf("ordinary prose classified as review metadata: %q", phrase)
		}
	}
	if !hasSubmittedActionableClaim("**Actionable comments posted**: 1") {
		t.Fatal("visible formatted count must remain a count")
	}
	if hasSubmittedLineActionableClaim("Preserve the actionable comments posted: check the label.") {
		t.Fatal("a phrase inside prose is not a standalone publication declaration")
	}
	if !hasSubmittedLineActionableClaim("- **Actionable comments posted**: 1") {
		t.Fatal("visible standalone count preceded by a list marker must remain a count")
	}
}

func TestStandingCountTreatsFencedExamplesAsParagraphBreaks(t *testing.T) {
	lines, sound := findingLines("Findings:\n```text\nsample releases\n```\n2 releases remain.")
	if !sound || standingInventoryContradictsZero(lines, &standingHistoricalParser{}) {
		t.Fatal("a fenced example cannot connect two separate prose paragraphs")
	}
}
