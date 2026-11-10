package reviewexec

import (
	"sort"
	"strings"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// CIWorkflowObservation names an already observed latest workflow run only.
// It neither proves which jobs were checked nor assesses earlier runs.
type CIWorkflowObservation struct {
	WorkflowID int64  `json:"workflow_id"`
	Basis      string `json:"basis"`
	RunID      int64  `json:"run_id,omitempty"`
	RunAttempt int64  `json:"run_attempt,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

// A non-strict authority can prove head CI while its cached test merge is
// unproven. Optional workflow facts must keep that exact selected head binding.
func ciWatchAllowsWorkflowCandidate(o reviewprovider.CIObservation) bool {
	return o.Candidate != nil && (o.Candidate.State == "complete" ||
		o.Selection == "head" && o.SelectedCommitSHA == o.Target.HeadSHA && reviewprovider.NonstrictHeadCICandidate(*o.Candidate, o.Target, o.Authority))
}

func ciWatchWorkflowObservations(o reviewprovider.CIObservation, ids []int64) []CIWorkflowObservation {
	if len(ids) == 0 || len(ids) > 16 || !ciWatchAllowsWorkflowCandidate(o) || o.Candidate.RepositoryID != o.Target.RepositoryID || o.Candidate.PullNumber != o.Target.PullNumber || o.Candidate.HeadSHA != o.Target.HeadSHA || o.Candidate.BaseSHA != o.Target.BaseSHA || o.Candidate.BaseRef != o.Target.BaseRef || o.State != o.Assessment.State || o.Reason != o.Assessment.Reason || o.Actions == nil || o.Actions.RepositoryID != o.Target.RepositoryID || !strings.EqualFold(o.Actions.Repository, o.Target.Repository) || o.Actions.HeadSHA != o.SelectedCommitSHA {
		return nil
	}
	latest, err := reviewprovider.LatestActionsRuns(*o.Actions)
	if err != nil {
		return nil
	}
	byID := map[int64]reviewprovider.ActionsHeadRun{}
	for _, run := range latest.Runs {
		byID[run.WorkflowID] = run
	}
	result := make([]CIWorkflowObservation, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return nil
		}
		seen[id] = true
		row := CIWorkflowObservation{WorkflowID: id, Basis: "latest_run_only"}
		if run, known := byID[id]; known {
			row.RunID, row.RunAttempt, row.Status, row.Conclusion = run.ID, run.RunAttempt, run.Status, run.Conclusion
		}
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].WorkflowID < result[j].WorkflowID })
	return result
}
