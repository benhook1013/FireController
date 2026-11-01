package reviewprovider

import (
	"context"
	"strings"
	"time"
)

// CIWorkflowFailure is optional attention for one explicitly selected workflow.
// It does not add required checks or change the required-gate assessment.
type CIWorkflowFailure struct {
	WorkflowID int64  `json:"workflow_id"`
	RunID      int64  `json:"run_id"`
	RunAttempt int64  `json:"run_attempt"`
	JobID      int64  `json:"job_id"`
	CheckRunID int64  `json:"check_run_id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

// CIWorkflowAttentionError preserves provider backoff without invalidating the
// separately completed required-gate observation returned alongside it.
type CIWorkflowAttentionError struct{ Cause error }

func (*CIWorkflowAttentionError) Error() string {
	return "Optional CI workflow attention could not be collected."
}
func (e *CIWorkflowAttentionError) Unwrap() error { return e.Cause }

// ObserveCIGateWithWorkflows shares the ordinary thirty-second collection
// budget. Optional failures never replace, invalidate or upgrade gate facts.
func (g *GitHub) ObserveCIGateWithWorkflows(ctx context.Context, target CITarget, gateRef string, workflowIDs []int64) (CIObservation, []CIWorkflowFailure, error) {
	failures := []CIWorkflowFailure{}
	wanted := make(map[int64]bool, len(workflowIDs))
	if ctx == nil || len(workflowIDs) > 16 {
		return CIObservation{}, failures, invalidSelector()
	}
	for _, id := range workflowIDs {
		if id <= 0 || wanted[id] {
			return CIObservation{}, failures, invalidSelector()
		}
		wanted[id] = true
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	observation, err := g.observeCI(ctx, target, gateRef)
	if err != nil || len(wanted) == 0 || observation.Actions == nil || observation.Candidate == nil || !matchesCITarget(*observation.Candidate, target) || observation.State != observation.Assessment.State || observation.Reason != observation.Assessment.Reason {
		return observation, failures, err
	}
	if observation.State == "satisfied" || observation.State == "not_required" || observation.State == "failed" {
		return observation, failures, nil
	}
	for _, check := range observation.Assessment.Checks {
		if check.State == "failed" {
			return observation, failures, nil
		}
	}
	actions := *observation.Actions
	if actions.RepositoryID != target.RepositoryID || !strings.EqualFold(actions.Repository, target.Repository) || actions.HeadSHA != observation.SelectedCommitSHA {
		return observation, failures, nil
	}
	latest, latestErr := LatestActionsRuns(actions)
	if latestErr != nil {
		return observation, failures, nil
	}
	for _, run := range latest.Runs {
		if !wanted[run.WorkflowID] || ctx.Err() != nil {
			continue
		}
		var attempt ActionsAttempt
		matches := 0
		for _, retained := range observation.Attempts {
			if matchingCIWorkflowAttempt(run, retained) {
				attempt, matches = retained, matches+1
			}
		}
		if matches > 1 {
			continue
		}
		if matches == 0 {
			attempt, err = g.CurrentActionsAttempt(ctx, actions.Repository, actions.HeadSHA, run.CheckSuiteID)
			if err != nil {
				return observation, failures, &CIWorkflowAttentionError{Cause: err}
			}
		}
		if failure, proven := projectCIWorkflowFailure(run, attempt); proven {
			failures = append(failures, failure)
		}
	}
	return observation, failures, nil
}

func matchingCIWorkflowAttempt(run ActionsHeadRun, attempt ActionsAttempt) bool {
	wire := githubActionsRun{ID: run.ID, HeadSHA: run.HeadSHA, CheckSuiteID: run.CheckSuiteID, WorkflowID: run.WorkflowID, RunAttempt: run.RunAttempt, RunNumber: run.RunNumber, Status: run.Status, Event: run.Event, CreatedAt: run.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: run.UpdatedAt.Format(time.RFC3339Nano)}
	if run.Status == "completed" || run.Conclusion != "" {
		wire.Conclusion = &run.Conclusion
	}
	wire.Repository.ID, wire.Repository.FullName = run.RepositoryID, run.Repository
	return validActionsRun(wire, run.Repository, run.HeadSHA, run.CheckSuiteID) && attempt.State == "complete" && attempt.Reason == "" && !attempt.ObservedAt.IsZero() && attempt.RepositoryID == run.RepositoryID && strings.EqualFold(attempt.Repository, run.Repository) && attempt.HeadSHA == run.HeadSHA && attempt.WorkflowID == run.WorkflowID && attempt.RunID == run.ID && attempt.RunNumber == run.RunNumber && attempt.RunAttempt == run.RunAttempt && attempt.CheckSuiteID == run.CheckSuiteID && attempt.Event == run.Event && attempt.Status == run.Status && attempt.Conclusion == run.Conclusion
}

// Inspect the complete inventory before selecting its lowest failed job ID;
// duplicate or malformed jobs cannot supply attention by enumeration order.
func projectCIWorkflowFailure(run ActionsHeadRun, attempt ActionsAttempt) (CIWorkflowFailure, bool) {
	var out CIWorkflowFailure
	if !matchingCIWorkflowAttempt(run, attempt) || attempt.Jobs == nil || len(attempt.Jobs) > githubMaxItems {
		return out, false
	}
	jobs, checks := map[int64]bool{}, map[int64]bool{}
	for _, job := range attempt.Jobs {
		if job.ID <= 0 || job.CheckRunID <= 0 || jobs[job.ID] || checks[job.CheckRunID] || job.HeadSHA != run.HeadSHA || !validPlainText(job.Name, 255) || strings.TrimSpace(job.Name) == "" || !validCIStateFacts(job.Status, job.Conclusion, job.StartedAt, job.CompletedAt) {
			return CIWorkflowFailure{}, false
		}
		jobs[job.ID], checks[job.CheckRunID] = true, true
		if job.Status != "completed" {
			continue
		}
		switch job.Conclusion {
		case "failure", "timed_out", "action_required", "startup_failure":
			if out.JobID == 0 || job.ID < out.JobID {
				out = CIWorkflowFailure{WorkflowID: run.WorkflowID, RunID: run.ID, RunAttempt: run.RunAttempt, JobID: job.ID, CheckRunID: job.CheckRunID, Name: job.Name, Conclusion: job.Conclusion}
			}
		}
	}
	return out, out.JobID > 0
}
