package reviewprovider

import (
	"strings"
	"testing"
)

func TestObserveRequiredCIReportsCandidateReasonWithoutResultCredit(t *testing.T) {
	for _, tc := range []struct{ mode, reason string }{
		{"stale_merge_parent", "ci_merge_base_mismatch"},
		{"cached_base_parent", "ci_merge_base_mismatch"},
		{"pending", "ci_merge_pending"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newCIObservationFixture(t, tc.mode)
			got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			if err != nil || got.State != "unproven" || got.Reason != tc.reason || got.Candidate == nil || got.Candidate.Reason != tc.reason || got.Results != nil || got.Actions != nil || got.SelectedCommitSHA != "" || got.Assessment.State != "" {
				t.Fatal("candidate diagnostic lost or unsafe result credit", got, err)
			}
			paths, counts := f.snapshot()
			if counts[candidatePullPath] != 2 || tc.mode != "pending" && (counts[candidateCommitPath] != 1 || counts[candidateBranchPath] != 2) || tc.mode == "pending" && counts[candidateCommitPath] != 0 {
				t.Fatal("candidate collection scope or bounded reads changed", counts)
			}
			for _, path := range paths {
				if strings.Contains(path, "/commits/") && strings.Contains(path, "/check-runs") || strings.Contains(path, "/actions/") {
					t.Fatal("unverified merge caused result fallback", path)
				}
			}
		})
	}
}
