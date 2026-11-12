package client

import (
	"strings"
	"testing"
)

func TestCIWatchImplicitBaseRetryAdvice(t *testing.T) {
	for _, code := range []string{"canceled", "deadline_exceeded", "overloaded"} {
		for _, base := range []string{"", strings.Repeat("b", 40)} {
			t.Run(code+"/"+base, func(t *testing.T) {
				original := &Error{Code: code, Action: "Original advice.", Details: map[string]any{"reason": "ci_watch_capacity"}}
				if code == "overloaded" {
					original.Status = 503
				}
				pins := []CIWatchTarget{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: base}}
				got := ciWatchTargetError(original, "", pins).(*Error)
				if base == "" {
					for _, text := range []string{"base was implicit", "Verify the original comparison", "supply explicit base pins"} {
						if !strings.Contains(got.Action, text) {
							t.Fatalf("missing %q: %s", text, got.Action)
						}
					}
				} else if got.Action != original.Action {
					t.Fatal("explicit advice changed")
				}
				if original.Action != "Original advice." || got.Code != original.Code || got.Status != original.Status {
					t.Fatal("original diagnostic mutated")
				}
			})
		}
	}
}

func TestCIWatchMismatchFactsRespectExplicitBase(t *testing.T) {
	for _, pinBase := range []string{"", strings.Repeat("b", 40), strings.Repeat("f", 40)} {
		t.Run(pinBase, func(t *testing.T) {
			head, observed := strings.Repeat("a", 40), strings.Repeat("b", 40)
			pin := CIWatchTarget{PullNumber: 2, HeadSHA: head, BaseSHA: pinBase}
			value := CIWatchResult{State: "pending", Targets: []CIWatchTargetResult{{PullNumber: 2, HeadSHA: head, BaseSHA: pinBase, ObservedHeadSHA: head, ObservedBaseSHA: observed, State: "pending", Reason: "ci_merge_base_mismatch", Checks: []ReviewCIContext{}, MergeBaseMismatch: &CIMergeBaseMismatch{PRBaseSHA: observed, LiveBaseSHA: strings.Repeat("d", 40), MergeSHA: strings.Repeat("e", 40), MergeBaseParentSHA: observed}}}}
			got := validCIWatchResult(value, []CIWatchTarget{pin})
			want := pinBase == "" || pinBase == observed
			if got != want {
				t.Fatalf("accepted=%v want=%v", got, want)
			}
		})
	}
}
