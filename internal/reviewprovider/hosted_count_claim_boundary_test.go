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

func TestHostedCountBoundariesRespectBlockAndHeadingMarkup(t *testing.T) {
	lines, sound := findingLines("Findings:\n<pre>\nExample release data\n</pre>\n2 releases remain.")
	if !sound || standingInventoryContradictsZero(lines, &standingHistoricalParser{}) {
		t.Fatal("unrelated prose on either side of an example was joined")
	}
	lines, sound = findingLines("<pre>\nFindings: 2\n</pre>")
	if !sound || !standingInventoryContradictsZero(lines, &standingHistoricalParser{}) {
		t.Fatal("preformatted count stopped contributing refusal-only evidence")
	}
	if !hasSubmittedLineActionableClaim("### Actionable comments posted: 1") {
		t.Fatal("visible counted heading is not ordinary prose")
	}
	if !hasBareSubmittedActionableLabel("## **Actionable comments posted**") {
		t.Fatal("visible standalone heading label is not ordinary prose")
	}
	lines, sound = findingLines("Current <strong>findings</strong>: 2")
	if !sound || !standingInventoryContradictsZero(lines, &standingHistoricalParser{}) {
		t.Fatal("decorative markup obscured an explicit count")
	}
}

func TestStandingMarkerScanContinuesAfterEditorialComment(t *testing.T) {
	for _, tc := range []struct {
		name, remainder string
		wantInvalid     bool
	}{
		{"editorial", "<!-- Formatting note. -->", false},
		{"reserved wording", "<!-- release notes by coderabbit.ai -->", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := &standingHistoricalParser{}
			standingSections("<!-- Editorial note.\n--> "+tc.remainder, history)
			if history.invalid != tc.wantInvalid {
				t.Fatalf("marker invalid = %v, want %v", history.invalid, tc.wantInvalid)
			}
		})
	}
}
