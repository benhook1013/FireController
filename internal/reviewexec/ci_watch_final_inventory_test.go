package reviewexec

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchWorkflowObservationsUseValidatedFinalInventory(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	o := sourceCITestObservation(target, "pending")
	old := reviewprovider.ActionsHeadRun{ID: 100, WorkflowID: 10, CheckSuiteID: 1000, RunNumber: 1, RunAttempt: 2, RepositoryID: target.RepositoryID, Repository: target.Repository, HeadSHA: target.HeadSHA, Event: "pull_request", Status: "completed", Conclusion: "failure", CreatedAt: o.ObservedAt.Add(-time.Hour), UpdatedAt: o.ObservedAt.Add(-time.Hour)}
	o.Actions.Runs = []reviewprovider.ActionsHeadRun{old}
	newer := old
	newer.ID, newer.CheckSuiteID, newer.RunNumber, newer.RunAttempt = 101, 1001, 2, 1
	newer.Status, newer.Conclusion = "in_progress", ""
	newer.CreatedAt, newer.UpdatedAt = o.ObservedAt.Add(-time.Minute), o.ObservedAt
	final := *o.Actions
	final.Runs = []reviewprovider.ActionsHeadRun{old, newer}
	for _, mode := range []string{"newer run", "empty final", "foreign repository", "foreign name", "foreign head", "invalid run"} {
		t.Run(mode, func(t *testing.T) {
			value := o
			current := final
			current.Runs = append([]reviewprovider.ActionsHeadRun(nil), final.Runs...)
			value.WorkflowActions = &current
			switch mode {
			case "empty final":
				current.Runs = []reviewprovider.ActionsHeadRun{}
			case "foreign repository":
				current.RepositoryID++
			case "foreign name":
				current.Repository = "other/repo"
			case "foreign head":
				current.HeadSHA = target.BaseSHA
			case "invalid run":
				current.Runs[1].RunAttempt = 0
			}
			before, finalBefore := encoded(value), encoded(value.WorkflowActions)
			got := ciWatchWorkflowObservations(value, []int64{10})
			var want []CIWorkflowObservation
			switch mode {
			case "newer run":
				want = []CIWorkflowObservation{{WorkflowID: 10, Basis: "latest_run_only", RunID: 101, RunAttempt: 1, Status: "in_progress"}}
			case "empty final":
				want = []CIWorkflowObservation{{WorkflowID: 10, Basis: "latest_run_only"}}
			}
			if !reflect.DeepEqual(got, want) || encoded(value) != before || encoded(value.WorkflowActions) != finalBefore {
				t.Fatal("latest details used stale/unbound inventory or changed facts", got, want)
			}
		})
	}
}
