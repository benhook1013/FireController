package reviewprovider

import "testing"

func TestHostedParserBenignCommentAndPreBoundaries(t *testing.T) {
	lines, sound := findingLines("<!-- explanatory note mentions <pre>\n continuation -->\nOrdinary text")
	if !sound || len(lines) != 3 || lines[0].preCountContinues || lines[2].countText != "Ordinary text" {
		t.Fatalf("a tag mentioned in a note changed prose visibility: %#v", lines)
	}
	lines, sound = findingLines("<pre>\n<!-- explanatory note\n</pre>\ncontinued note -->\n</pre>\nOrdinary text")
	if !sound || len(lines) != 6 || !lines[2].preCountContinues || lines[5].countText != "Ordinary text" {
		t.Fatalf("preformatted note changed block boundaries: %#v", lines)
	}
}

func TestHostedParserOrderedParagraphBoundaries(t *testing.T) {
	for _, tc := range []struct {
		line       string
		interrupts bool
	}{{"1. Read the notes", true}, {"1) Read the notes", true}, {"2. Read the notes", false}, {"1.   ", false}, {"1st item", false}} {
		if got := submittedOrderedParagraphInterrupt.MatchString(tc.line); got != tc.interrupts {
			t.Errorf("%q interruption=%t, want %t", tc.line, got, tc.interrupts)
		}
	}
}
