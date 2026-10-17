package reviewprovider

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// LatestActionsRuns selects the greatest provider run number independently for
// each workflow in a complete, bounded head inventory. It preserves pending,
// failed and manually triggered runs: selection establishes neither event
// applicability nor required-check success, and does not carry jobs forward.
func LatestActionsRuns(in ActionsHeadInventory) (ActionsHeadInventory, error) {
	if !validRepositorySelector(in.Repository) || in.RepositoryID <= 0 || !validSHA(in.HeadSHA) || strings.ToLower(in.HeadSHA) != in.HeadSHA {
		return ActionsHeadInventory{}, invalidSelector()
	}
	if _, valid := actionsTime(in.ObservedAt.Format(time.RFC3339Nano)); !valid || in.Runs == nil {
		return ActionsHeadInventory{}, invalidResponse(false, http.StatusOK)
	}
	if len(in.Runs) >= 1000 {
		return ActionsHeadInventory{}, ciInventoryIncomplete()
	}
	ids := make(map[int64]bool, len(in.Runs))
	suites := make(map[int64]bool, len(in.Runs))
	numbers := make(map[[2]int64]bool, len(in.Runs))
	latest := make(map[int64]ActionsHeadRun)
	for _, run := range in.Runs {
		var conclusion *string
		if run.Status == "completed" || run.Conclusion != "" {
			conclusion = &run.Conclusion
		}
		wire := githubActionsRun{ID: run.ID, HeadSHA: run.HeadSHA, CheckSuiteID: run.CheckSuiteID, WorkflowID: run.WorkflowID, RunAttempt: run.RunAttempt, RunNumber: run.RunNumber, Status: run.Status, Conclusion: conclusion, Event: run.Event, CreatedAt: run.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: run.UpdatedAt.Format(time.RFC3339Nano)}
		wire.Repository.ID, wire.Repository.FullName = run.RepositoryID, run.Repository
		number := [2]int64{run.WorkflowID, run.RunNumber}
		if !validActionsRun(wire, in.Repository, in.HeadSHA, run.CheckSuiteID) || run.RepositoryID != in.RepositoryID || ids[run.ID] || suites[run.CheckSuiteID] || numbers[number] {
			return ActionsHeadInventory{}, invalidResponse(false, http.StatusOK)
		}
		ids[run.ID], suites[run.CheckSuiteID], numbers[number] = true, true, true
		if previous, exists := latest[run.WorkflowID]; !exists || run.RunNumber > previous.RunNumber {
			latest[run.WorkflowID] = run
		}
	}
	out := in
	out.Runs = make([]ActionsHeadRun, 0, len(latest))
	for _, run := range latest {
		out.Runs = append(out.Runs, run)
	}
	sort.Slice(out.Runs, func(i, j int) bool { return out.Runs[i].WorkflowID < out.Runs[j].WorkflowID })
	return out, nil
}
