package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func candidatePhasesFixture(t *testing.T) (githubCIPull, githubCIBranch, githubCIMerge) {
	t.Helper()
	decode := func(in any, out any) {
		t.Helper()
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if err = decodeProviderJSON(b, out); err != nil {
			t.Fatal(err)
		}
	}
	var first githubCIPull
	var branch githubCIBranch
	var object githubCIMerge
	decode(candidatePullFixture(), &first)
	decode(candidateBranchFixture(), &branch)
	decode(candidateCommitFixture(candidateBase, candidateHead), &object)
	return first, branch, object
}
func newCandidatePhasesFixture() *ciCandidatePhases {
	return newCICandidatePhases("owner/repo", 20, 7, candidateHead)
}

func TestCICandidatePhasesPreserveFinalWitnessAndClassification(t *testing.T) {
	for _, mode := range []string{"verified", "reversed", "pending", "conflicting", "base_mismatch", "base_moves", "final_moves", "after_missing", "wrong_object"} {
		t.Run(mode, func(t *testing.T) {
			first, before, object := candidatePhasesFixture(t)
			switch mode {
			case "pending":
				first.Mergeable = nil
			case "conflicting":
				v := false
				first.Mergeable = &v
			case "reversed":
				object.Parents[0], object.Parents[1] = object.Parents[1], object.Parents[0]
			case "base_mismatch":
				before.Commit.SHA = strings.Repeat("d", 40)
			case "wrong_object":
				object.SHA = strings.Repeat("e", 40)
			}
			p := newCandidatePhasesFixture()
			p.acceptFirst(first, nil)
			cache := map[string]githubCIMerge{}
			if p.phase == ciCandidateBefore {
				p.acceptBefore(before, true, nil)
				p.acceptObject(object, nil, cache)
				after := before
				if mode == "base_moves" {
					after.Commit.SHA = strings.Repeat("e", 40)
				}
				p.acceptAfter(after, mode != "after_missing", nil)
			}
			if p.phase != ciCandidateFinal || p.result.State == "complete" || p.result.MergeBaseMismatch != nil {
				t.Fatal("credited before final PR", p)
			}
			last := first
			if mode == "final_moves" {
				last.Pull.Head.SHA = strings.Repeat("e", 40)
			}
			p.acceptFinal(t.Context(), last, nil)
			if p.phase != ciCandidateDone || p.err != nil || !p.result.ObservedAt.IsZero() {
				t.Fatal("final state/timestamp ownership", p)
			}
			switch mode {
			case "verified", "reversed":
				if p.result.State != "complete" || p.result.MergeState != "verified" || p.result.MergeSHA != candidateMerge || p.result.MergeBaseParentSHA != candidateBase {
					t.Fatal(p.result)
				}
			case "conflicting":
				if p.result.State != "complete" || p.result.MergeState != "conflicting" || p.result.MergeSHA != "" {
					t.Fatal(p.result)
				}
			case "pending":
				if p.result.State != "unproven" || p.result.Reason != "ci_merge_pending" {
					t.Fatal(p.result)
				}
			case "base_mismatch":
				m := p.result.MergeBaseMismatch
				if p.result.Reason != "ci_merge_base_mismatch" || m == nil || m.PRBaseSHA != candidateBase || m.LiveBaseSHA != before.Commit.SHA || m.MergeBaseParentSHA != candidateBase || p.result.MergeSHA != "" {
					t.Fatal(p.result)
				}
			case "final_moves":
				if p.result.Reason != "ci_candidate_changed" || p.result.MergeBaseMismatch != nil {
					t.Fatal(p.result)
				}
			default:
				if p.result.State != "unproven" || p.result.Reason != "ci_merge_identity_unproven" || p.result.MergeSHA != "" {
					t.Fatal(p.result)
				}
			}
			_, cached := cache[candidateMerge]
			if cached != (mode != "pending" && mode != "conflicting" && mode != "wrong_object") {
				t.Fatal("immutable cache admission", cache)
			}
		})
	}
}

func TestCICandidatePhasesEarlyRefusalAndReadErrors(t *testing.T) {
	for _, stage := range []ciCandidatePhase{ciCandidateFirst, ciCandidateBefore, ciCandidateObject, ciCandidateAfter, ciCandidateFinal} {
		first, branch, object := candidatePhasesFixture(t)
		p := newCandidatePhasesFixture()
		failure := errors.New("synthetic read failure")
		maybe := func(want ciCandidatePhase) error {
			if stage == want {
				return failure
			}
			return nil
		}
		p.acceptFirst(first, maybe(ciCandidateFirst))
		if p.phase == ciCandidateBefore {
			p.acceptBefore(branch, true, maybe(ciCandidateBefore))
		}
		if p.phase == ciCandidateObject {
			p.acceptObject(object, maybe(ciCandidateObject), nil)
		}
		if p.phase == ciCandidateAfter {
			p.acceptAfter(branch, true, maybe(ciCandidateAfter))
		}
		if p.phase == ciCandidateFinal {
			p.acceptFinal(t.Context(), first, maybe(ciCandidateFinal))
		}
		if p.phase != ciCandidateDone || p.err != failure || p.result.State == "complete" {
			t.Fatal("read failure changed or credited", stage, p)
		}
	}
	for _, mode := range []string{"target", "missing_before", "invalid_object", "out_of_order", "cancel_final"} {
		t.Run(mode, func(t *testing.T) {
			first, branch, object := candidatePhasesFixture(t)
			p := newCandidatePhasesFixture()
			if mode == "target" {
				first.Pull.Head.SHA = strings.Repeat("d", 40)
			}
			if mode == "out_of_order" {
				p.acceptBefore(branch, true, nil)
			} else {
				p.acceptFirst(first, nil)
			}
			if p.phase == ciCandidateBefore {
				p.acceptBefore(branch, mode != "missing_before", nil)
			}
			if mode == "invalid_object" {
				object.SHA = "invalid"
			}
			if p.phase == ciCandidateObject {
				p.acceptObject(object, nil, nil)
			}
			if p.phase == ciCandidateAfter {
				p.acceptAfter(branch, true, nil)
			}
			if p.phase == ciCandidateFinal {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				p.acceptFinal(ctx, first, nil)
			}
			if p.phase != ciCandidateDone || p.result.State == "complete" {
				t.Fatal(p)
			}
			switch mode {
			case "target", "missing_before":
				if p.err != nil {
					t.Fatal(p.err)
				}
			case "invalid_object":
				var e *Error
				if !errors.As(p.err, &e) || e.Code != "provider_invalid_response" || e.Status != http.StatusOK {
					t.Fatal(p.err)
				}
			default:
				if p.err == nil {
					t.Fatal("missing refusal error")
				}
			}
		})
	}
}
