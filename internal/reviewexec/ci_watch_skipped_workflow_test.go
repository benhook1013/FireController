package reviewexec

import (
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchAcceptsOnlyProvenCurrentFailureThroughSkippedRun(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	o := sourceCITestObservation(target, "pending")
	older := reviewprovider.ActionsHeadRun{ID: 100, WorkflowID: 10, CheckSuiteID: 1000, RunNumber: 1, RunAttempt: 1, RepositoryID: target.RepositoryID, Repository: target.Repository, HeadSHA: o.SelectedCommitSHA, Event: "pull_request", Status: "completed", Conclusion: "failure", CreatedAt: o.ObservedAt.Add(-2 * time.Minute), UpdatedAt: o.ObservedAt.Add(-time.Minute)}
	newer := older
	newer.ID, newer.CheckSuiteID, newer.RunNumber, newer.Conclusion = 101, 1001, 2, "success"
	newer.CreatedAt, newer.UpdatedAt = o.ObservedAt.Add(-time.Minute), o.ObservedAt
	o.Actions.Runs = []reviewprovider.ActionsHeadRun{older, newer}
	attempt := func(run reviewprovider.ActionsHeadRun, jobID, checkID int64, conclusion string) reviewprovider.ActionsAttempt {
		return reviewprovider.ActionsAttempt{State: "complete", RepositoryID: run.RepositoryID, Repository: run.Repository, HeadSHA: run.HeadSHA, RunID: run.ID, RunNumber: run.RunNumber, RunAttempt: run.RunAttempt, WorkflowID: run.WorkflowID, CheckSuiteID: run.CheckSuiteID, Event: run.Event, Status: run.Status, Conclusion: run.Conclusion, ObservedAt: o.ObservedAt, Jobs: []reviewprovider.ActionsJob{{ID: jobID, CheckRunID: checkID, HeadSHA: run.HeadSHA, Name: "Failed leaf", Status: "completed", Conclusion: conclusion, StartedAt: run.CreatedAt, CompletedAt: run.UpdatedAt}}}
	}
	o.WorkflowAttempts = []reviewprovider.ActionsAttempt{attempt(older, 1000, 10000, "failure"), attempt(newer, 1001, 10001, "skipped")}
	current := *o.Actions
	o.WorkflowActions = &current
	row := reviewprovider.CIWorkflowFailure{WorkflowID: 10, RunID: 100, RunAttempt: 1, JobID: 1000, CheckRunID: 10000, Name: "Failed leaf", Conclusion: "failure"}
	before := encoded(o)
	if rows := ciWatchValidatedWorkflowFailures(o, []int64{10}, []reviewprovider.CIWorkflowFailure{row}); len(rows) != 1 || rows[0] != row || encoded(o) != before {
		t.Fatal("valid older current execution hidden or gate facts mutated", rows)
	}
	for _, damage := range []string{"proof", "recheck", "job", "foreign-workflow", "current-attempt-repaired"} {
		t.Run(damage, func(t *testing.T) {
			value, failure := o, row
			switch damage {
			case "proof":
				value.WorkflowAttempts = nil
			case "recheck":
				value.WorkflowActions = nil
			case "job":
				failure.JobID++
			case "foreign-workflow":
				failure.WorkflowID++
			case "current-attempt-repaired":
				actions := *o.WorkflowActions
				actions.Runs = append([]reviewprovider.ActionsHeadRun(nil), actions.Runs...)
				actions.Runs[0].RunAttempt++
				value.WorkflowActions = &actions
			}
			if rows := ciWatchValidatedWorkflowFailures(value, []int64{10}, []reviewprovider.CIWorkflowFailure{failure}); len(rows) != 0 {
				t.Fatal("unproven earlier execution accepted", rows)
			}
		})
	}
}
