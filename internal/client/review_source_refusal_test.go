package client

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSourceRefusalRejectsUnboundOrMalformedComparisonMetadata(t *testing.T) {
	valid := ReviewSourceRefusal{PullNumber: 4, ParentPullNumber: 6, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}
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

func TestPredecessorUnfinishedDiagnosticUsesOnlyRetainedFacts(t *testing.T) {
	for _, cause := range []string{"not_in_queue", "discovery_incomplete", "incoming_route_pending", ""} {
		var facts *ReviewSourceRefusal
		if cause != "" {
			facts = &ReviewSourceRefusal{PullNumber: 6, Cause: cause}
		}
		full := AgentReviewOperation(ReviewOperation{Channel: "cli", State: "held", Reason: "predecessor_unfinished", SourceRefusal: facts})
		compact := AgentReviewOperationStatus(ReviewOperationStatus{Channel: "cli", State: "held", Reason: "predecessor_unfinished", SourceRefusal: facts})
		for _, hint := range []*ReviewOperationDiagnostic{full.Diagnostic, compact.Diagnostic} {
			if hint == nil || !strings.Contains(hint.Explanation, "before provider dispatch") || !strings.Contains(hint.NextAction, "own source, evidence, policy and provider guards") {
				t.Fatal("missing guarded dependency guidance", cause, hint)
			}
			if cause == "" {
				if !strings.Contains(hint.Explanation, "not retained") || strings.Contains(hint.NextAction, "reviews pull 6") {
					t.Fatal("invented historical blocker")
				}
			} else {
				if !strings.Contains(hint.Explanation, "PR #6") || !strings.Contains(hint.NextAction, "reviews pull 6") || !strings.Contains(hint.NextAction, "--channel cli") {
					t.Fatal("missing original predecessor/current next read")
				}
				if cause == "discovery_incomplete" && !strings.Contains(hint.Explanation, "not necessarily findings") {
					t.Fatal("incomplete discovery misrepresented as findings")
				}
			}
		}
	}
}

func TestSelectedParentRefusalStrictBindingAndOriginalDiagnostics(t *testing.T) {
	selected := ReviewSourceRefusal{PullNumber: 2, ParentPullNumber: 6, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}
	if !validReviewSourceRefusal(&selected, "held", "parent_moved", 2) || !validReviewSourceRefusal(nil, "held", "parent_moved", 2) {
		t.Fatal("valid selected mismatch or historical omission refused")
	}
	for _, mutate := range []func(*ReviewSourceRefusal){
		func(v *ReviewSourceRefusal) { v.PullNumber = 4 }, func(v *ReviewSourceRefusal) { v.ParentPullNumber = 2 }, func(v *ReviewSourceRefusal) { v.ParentPullNumber = 0 }, func(v *ReviewSourceRefusal) { v.ObservedBaseSHA = "short" }, func(v *ReviewSourceRefusal) { v.ExpectedParentSHA = strings.Repeat("A", 40) }, func(v *ReviewSourceRefusal) { v.ExpectedParentSHA = v.ObservedBaseSHA }, func(v *ReviewSourceRefusal) { v.BaseRef = "branch" }, func(v *ReviewSourceRefusal) { v.Cause = "discovery_incomplete" },
	} {
		v := selected
		mutate(&v)
		if validReviewSourceRefusal(&v, "held", "parent_moved", 2) {
			t.Fatal("invalid selected comparison accepted", v)
		}
	}
	if validReviewSourceRefusal(&selected, "held", "predecessor_parent_moved", 2) || validReviewSourceRefusal(&selected, "active", "parent_moved", 2) {
		t.Fatal("metadata crossed refusal/state")
	}
	for _, reason := range []string{"parent_moved", "predecessor_parent_moved"} {
		v := selected
		checked := "selected PR #2"
		if reason == "predecessor_parent_moved" {
			v.PullNumber = 4
			checked = "ancestor PR #4"
		}
		full := ReviewOperation{ID: "original-operation", PullNumber: 2, Channel: "hosted", State: "held", Reason: reason, SourceRefusal: &v, HeadSHA: strings.Repeat("c", 40)}
		compact := ReviewOperationStatus{ID: full.ID, PullNumber: 2, Channel: full.Channel, State: full.State, Reason: reason, SourceRefusal: &v}
		f, c := AgentReviewOperation(full), AgentReviewOperationStatus(compact)
		if !reflect.DeepEqual(f.ReviewOperation, full) || !reflect.DeepEqual(c.ReviewOperationStatus, compact) || !reflect.DeepEqual(f.Diagnostic, c.Diagnostic) {
			t.Fatal("original comparison or view changed", f, c)
		}
		if f.Diagnostic == nil {
			t.Fatal("missing diagnostic")
		}
		for _, phrase := range []string{checked, fmt.Sprintf("parent PR #%d", v.ParentPullNumber), v.ObservedBaseSHA, v.ExpectedParentSHA, "original observed base", "direct-parent head", "recorded at the check", "not a current observation"} {
			if !strings.Contains(f.Diagnostic.Explanation, phrase) {
				t.Fatal("retained comparison not explained", reason, f.Diagnostic)
			}
		}
	}
}
