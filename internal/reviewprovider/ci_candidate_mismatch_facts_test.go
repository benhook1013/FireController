package reviewprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCICandidateMismatchFactsUseActualExistingReads(t *testing.T) {
	for _, mode := range []string{"cached_base_parent", "stale_merge_parent", "pending", "conflicting", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			f := newCIObservationFixture(t, mode)
			got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			if err != nil || got.Candidate == nil {
				t.Fatal(got, err)
			}
			want := &CIMergeBaseMismatch{PRBaseSHA: candidateBase, LiveBaseSHA: candidateBase, MergeSHA: candidateMerge, MergeBaseParentSHA: strings.Repeat("d", 40)}
			switch mode {
			case "cached_base_parent":
				want.LiveBaseSHA, want.MergeBaseParentSHA = strings.Repeat("d", 40), candidateBase
			case "stale_merge_parent":
			default:
				if got.Candidate.MergeBaseMismatch != nil {
					t.Fatal("non-mismatch acquired mismatch proof", got.Candidate)
				}
				return
			}
			if got.State != "unproven" || got.SelectedCommitSHA != "" || got.Results != nil || got.Actions != nil || got.Candidate.MergeSHA != "" || !reflect.DeepEqual(got.Candidate.MergeBaseMismatch, want) {
				t.Fatal("mismatch identities lost or gained CI credit", got, want)
			}
			_, calls := f.snapshot()
			if calls[candidatePullPath] != 2 || calls[candidateBranchPath] != 2 || calls[candidateCommitPath] != 1 {
				t.Fatal("diagnostic added candidate reads", calls)
			}
		})
	}
}

func TestCICandidateMismatchFactsAbsentAfterSourceMovement(t *testing.T) {
	for _, movement := range []string{"pr", "branch"} {
		t.Run(movement, func(t *testing.T) {
			var pulls, branches atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				switch r.URL.Path {
				case candidatePullPath:
					body = candidatePullFixture()
					if pulls.Add(1) == 2 && movement == "pr" {
						body["head"].(map[string]any)["sha"] = strings.Repeat("e", 40)
					}
				case candidateBranchPath:
					body = candidateBranchFixture()
					body["commit"].(map[string]any)["sha"] = strings.Repeat("d", 40)
					if branches.Add(1) == 2 && movement == "branch" {
						body["commit"].(map[string]any)["sha"] = strings.Repeat("e", 40)
					}
				case candidateCommitPath:
					body = candidateCommitFixture(candidateBase, candidateHead)
				default:
					t.Error("unexpected candidate read", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
			if err != nil || got.State != "unproven" || got.Reason == "ci_merge_base_mismatch" || got.MergeBaseMismatch != nil || pulls.Load() != 2 || branches.Load() != 2 {
				t.Fatal("unstable source supplied stable mismatch identities", got, err)
			}
		})
	}
}
