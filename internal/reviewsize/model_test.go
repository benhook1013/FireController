package reviewsize

import (
	"strings"
	"testing"
)

func TestIndependentLimitsAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name              string
		limits            Limits
		files             int64
		warning           bool
		remaining, excess int64
	}{
		{"disabled", Limits{}, 96, false, -1, -1},
		{"suggestion only", Limits{Suggested: 80}, 96, true, -1, -1},
		{"maximum only", Limits{Maximum: 100}, 96, false, 4, -1},
		{"combined", Limits{80, 100}, 96, true, 4, -1},
		{"at suggestion", Limits{80, 100}, 80, false, 20, -1},
		{"at maximum", Limits{80, 100}, 100, true, 0, -1},
		{"over maximum", Limits{80, 100}, 104, true, -1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zero := int64(0)
			a := Assess(7, tc.limits, &tc.files, &zero, 0)
			if tc.limits == (Limits{}) {
				if a != nil {
					t.Fatal(a)
				}
				return
			}
			if a == nil || !Valid(*a, 7) || a.AboveSuggested != tc.warning {
				t.Fatal(a)
			}
			if tc.remaining < 0 {
				if a.RemainingFiles != nil {
					t.Fatal(a)
				}
			} else if a.RemainingFiles == nil || *a.RemainingFiles != tc.remaining {
				t.Fatal(a)
			}
			if tc.excess < 0 {
				if a.ExcessFiles != nil {
					t.Fatal(a)
				}
			} else if a.ExcessFiles == nil || *a.ExcessFiles != tc.excess {
				t.Fatal(a)
			}
			if tc.limits.Maximum == 0 && (strings.Contains(a.Message, "remaining") || strings.Contains(a.Message, "maximum review size")) {
				t.Fatal(a.Message)
			}
		})
	}
}

func TestUnknownCountsAndHistoricalContext(t *testing.T) {
	a := Assess(7, Limits{80, 100}, nil, nil, 0)
	if !Valid(*a, 7) || a.AboveSuggested || a.RemainingFiles != nil || strings.Contains(a.Message, "No completed reviews") {
		t.Fatal(a)
	}
	files, completed := int64(96), int64(6)
	a = Assess(7, Limits{80, 100}, &files, &completed, 8)
	if !Valid(*a, 7) || !strings.Contains(a.Message, "6 completed reviews (historical)") || strings.Contains(a.Message, "before starting review") {
		t.Fatal(a)
	}
	wrong := int64(5)
	a.RemainingFiles = &wrong
	if Valid(*a, 7) {
		t.Fatal("inconsistent headroom accepted")
	}
	if !Valid(*Unavailable(7), 7) {
		t.Fatal("unavailable information must remain representable")
	}
}
