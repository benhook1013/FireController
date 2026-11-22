package reviewprovider

import "testing"

func TestPreformattedProjectionKeepsHiddenBoundaryMetadata(t *testing.T) {
	lines, ok := findingLines("<pre>\nAlpha<!-- editorial note\nmore note -->Beta\nGamma\n</pre>\n")
	if !ok || len(lines) != 5 {
		t.Fatal("unexpected line framing", ok, len(lines))
	}
	if got := lines[1]; got.preCountText != "Alpha" || !got.preCommentContinues || !got.preCountContinues || !got.hidden {
		t.Fatalf("unexpected first preformatted projection: %#v", got)
	}
	if got := lines[2]; got.preCountText != "Beta" || got.preCommentContinues || !got.preCountContinues || !got.hidden {
		t.Fatalf("unexpected completed preformatted projection: %#v", got)
	}
	if got := lines[3]; got.preCountText != "Gamma" || got.preCommentContinues {
		t.Fatalf("ordinary boundary was discarded: %#v", got)
	}
}

func TestRefusalReferenceProjectionOnlyChangesTheLabelView(t *testing.T) {
	raw := "See [release notes][guide] and [appendix][]."
	visible := submittedCountMarkdownReference.ReplaceAllString(raw, "${1}")
	if visible != "See release notes and appendix." || raw != "See [release notes][guide] and [appendix][]." {
		t.Fatalf("unexpected reference projection: %q", visible)
	}
}
