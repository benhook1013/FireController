package client

import (
	"strings"
	"testing"
)

func TestSourceRefusalRejectsUnboundOrMalformedComparisonMetadata(t *testing.T) {
	valid := ReviewSourceRefusal{4, 6, strings.Repeat("a", 40), strings.Repeat("b", 40), ""}
	if !validReviewSourceRefusal(nil, "held", "predecessor_parent_moved", 2) || !validReviewSourceRefusal(&valid, "held", "predecessor_parent_moved", 2) {
		t.Fatal("legacy omission or original metadata rejected")
	}
	for _, change := range []func(*ReviewSourceRefusal){
		func(v *ReviewSourceRefusal) { v.PullNumber = 2 },
		func(v *ReviewSourceRefusal) { v.PullNumber = 0 },
		func(v *ReviewSourceRefusal) { v.ParentPullNumber = v.PullNumber },
		func(v *ReviewSourceRefusal) { v.ObservedBaseSHA = "short" },
		func(v *ReviewSourceRefusal) { v.ExpectedParentSHA = strings.Repeat("G", 40) },
		func(v *ReviewSourceRefusal) { v.ExpectedParentSHA = v.ObservedBaseSHA },
	} {
		v := valid
		change(&v)
		if validReviewSourceRefusal(&v, "held", "predecessor_parent_moved", 2) {
			t.Fatal("invalid source diagnostic accepted", v)
		}
	}
	if validReviewSourceRefusal(&valid, "active", "predecessor_parent_moved", 2) || validReviewSourceRefusal(&valid, "held", "parent_moved", 2) {
		t.Fatal("source metadata attached to another state or refusal")
	}
}

func TestMissingParentRefusalVariantRejectsHybrids(t *testing.T) {
	v := ReviewSourceRefusal{PullNumber: 2, BaseRef: "codex/former-parent"}
	if !validReviewSourceRefusal(&v, "held", "parent_unavailable", 2) {
		t.Fatal("checked target or original branch rejected")
	}
	for _, mutate := range []func(*ReviewSourceRefusal){
		func(v *ReviewSourceRefusal) { v.BaseRef = "bad\nbranch" }, func(v *ReviewSourceRefusal) { v.ParentPullNumber = 3 }, func(v *ReviewSourceRefusal) { v.ExpectedParentSHA = strings.Repeat("a", 40) }, func(v *ReviewSourceRefusal) { v.BaseRef = strings.Repeat("a", 600) },
	} {
		bad := v
		mutate(&bad)
		if validReviewSourceRefusal(&bad, "held", "parent_unavailable", 2) {
			t.Fatal("hybrid accepted", bad)
		}
	}
	got := AgentReviewOperationStatus(ReviewOperationStatus{State: "held", Reason: "parent_unavailable", SourceRefusal: &v})
	if got.Diagnostic == nil || !strings.Contains(got.Diagnostic.Explanation, "PR #2") || !strings.Contains(got.Diagnostic.Explanation, v.BaseRef) {
		t.Fatal("diagnostic lacks original lookup", got)
	}
}
