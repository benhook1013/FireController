package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAgentPredecessorParentRecoveryUsesSelectedRequestOnly(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, withFacts := range []bool{false, true} {
			var facts *ReviewSourceRefusal
			if withFacts {
				facts = &ReviewSourceRefusal{PullNumber: 3368, ParentPullNumber: 3367, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}
			}
			full := ReviewOperation{ID: "original_operation", ProjectID: "original_project", PullNumber: 3369, Channel: channel, State: "held", Reason: "predecessor_parent_moved", HeadSHA: strings.Repeat("c", 40), ConfigurationRevision: 12, Revision: 3, SourceRefusal: facts}
			compact := ReviewOperationStatus{ID: full.ID, PullNumber: full.PullNumber, Channel: channel, State: full.State, Reason: full.Reason, Revision: full.Revision, SourceRefusal: facts, NextAction: "read_diagnostics"}
			f, c := AgentReviewOperation(full), AgentReviewOperationStatus(compact)
			if !reflect.DeepEqual(f.ReviewOperation, full) || !reflect.DeepEqual(c.ReviewOperationStatus, compact) || !reflect.DeepEqual(f.Diagnostic, c.Diagnostic) || f.Diagnostic == nil {
				t.Fatal("recovery changed retained facts or disagreed", f, c)
			}
			for _, phrase := range []string{"currently verified stable selected comparison", "current review authorization", "fresh key/current head and configuration", "reviews request 3369 --out-of-order", "pull_number:3369 and allow_out_of_order:true", "Ancestor republishing is not required", "own source/direct parent, findings/routes, policy and provider guards still apply"} {
				if !strings.Contains(f.Diagnostic.NextAction, phrase) {
					t.Fatal("conditional recovery omitted", phrase, f.Diagnostic)
				}
			}
			for _, bad := range []string{"reviews request 3368", "pull_number:3368", "reviews request 3367", "pull_number:3367"} {
				if strings.Contains(f.Diagnostic.NextAction, bad) {
					t.Fatal("request used ancestor identity", f.Diagnostic)
				}
			}
			if withFacts && !strings.Contains(f.Diagnostic.Explanation, "ancestor PR #3368 against parent PR #3367") {
				t.Fatal("original retained edge lost", f.Diagnostic)
			}
			for _, output := range []any{f, c} {
				body, err := json.Marshal(output)
				if err != nil || len(body) > 3072 {
					t.Fatal("recovery became unbounded", len(body), err)
				}
			}
			for _, state := range []string{"active", "completed"} {
				full.State, compact.State = state, state
				if AgentReviewOperation(full).Diagnostic != nil || AgentReviewOperationStatus(compact).Diagnostic != nil {
					t.Fatal("non-held operation gained bypass advice", state)
				}
			}
			full.State, compact.State = "held", "held"
			full.Reason, compact.Reason = "parent_moved", "parent_moved"
			if withFacts {
				facts.PullNumber = 3369
			}
			for _, hint := range []*ReviewOperationDiagnostic{AgentReviewOperation(full).Diagnostic, AgentReviewOperationStatus(compact).Diagnostic} {
				if hint == nil || strings.Contains(hint.NextAction, "--out-of-order") || strings.Contains(hint.NextAction, "allow_out_of_order:true") {
					t.Fatal("own/legacy move gained ancestor bypass", hint)
				}
			}
		}
	}
}
