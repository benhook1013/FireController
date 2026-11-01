package reviewprovider

import (
	"context"
	"sort"
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
	attempts := append([]ActionsAttempt(nil), observation.Attempts...)
	fallback := map[int64]bool{}
	withoutUnrecheckedFallback := func() []CIWorkflowFailure {
		kept := failures[:0]
		for _, failure := range failures {
			if !fallback[failure.WorkflowID] {
				kept = append(kept, failure)
			}
		}
		return kept
	}
	for _, newest := range latest.Runs {
		if !wanted[newest.WorkflowID] || ctx.Err() != nil {
			continue
		}
		runs := ciWorkflowRuns(actions, newest.WorkflowID)
		for index, run := range runs {
			var attempt ActionsAttempt
			matches := 0
			for _, retained := range attempts {
				if matchingCIWorkflowAttempt(run, retained) {
					attempt, matches = retained, matches+1
				}
			}
			if matches > 1 {
				break
			}
			if matches == 0 {
				if len(attempts) >= 32 {
					return observation, withoutUnrecheckedFallback(), &CIWorkflowAttentionError{Cause: ciInventoryIncomplete()}
				}
				attempt, err = g.CurrentActionsAttempt(ctx, actions.Repository, actions.HeadSHA, run.CheckSuiteID)
				if err != nil {
					return observation, withoutUnrecheckedFallback(), &CIWorkflowAttentionError{Cause: err}
				}
				attempts = append(attempts, attempt)
				observation.WorkflowAttempts = append(observation.WorkflowAttempts, attempt)
			}
			if !validCIWorkflowJobs(run, attempt) {
				break
			}
			failure, proven, canExtend := ciWorkflowFailureThroughSkips(runs[:index+1], attempts)
			if proven {
				failures = append(failures, failure)
				fallback[failure.WorkflowID] = index > 0
				break
			}
			if !canExtend {
				break
			}
		}
	}
	needsRecheck := false
	for _, older := range fallback {
		needsRecheck = needsRecheck || older
	}
	if needsRecheck {
		current, readErr := g.ActionsRunsForHead(ctx, actions.Repository, actions.RepositoryID, actions.HeadSHA)
		kept := failures[:0]
		for _, failure := range failures {
			if !fallback[failure.WorkflowID] || readErr == nil && sameCIWorkflowChain(actions, current, failure) {
				kept = append(kept, failure)
			}
		}
		failures = kept
		if readErr != nil {
			return observation, failures, &CIWorkflowAttentionError{Cause: readErr}
		}
		observation.WorkflowActions = &current
	}
	return observation, failures, nil
}

func ciWorkflowRuns(actions ActionsHeadInventory, workflow int64) []ActionsHeadRun {
	runs := []ActionsHeadRun{}
	for _, run := range actions.Runs {
		if run.WorkflowID == workflow {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunNumber > runs[j].RunNumber })
	return runs
}

func ciFailedJob(job ActionsJob) bool {
	if job.Status != "completed" {
		return false
	}
	switch job.Conclusion {
	case "failure", "timed_out", "action_required", "startup_failure":
		return true
	}
	return false
}

// Only an explicitly skipped same-name job can leave earlier execution relevant.
// Missing, duplicate/matrix-ambiguous, pending or cancelled jobs supply no bridge.
func ciSkippedWorkflowNames(run ActionsHeadRun, attempt ActionsAttempt) map[string]bool {
	if run.Status != "completed" || run.Conclusion != "success" && run.Conclusion != "neutral" && run.Conclusion != "skipped" {
		return nil
	}
	counts := map[string]int{}
	for _, job := range attempt.Jobs {
		counts[job.Name]++
	}
	result := map[string]bool{}
	for _, job := range attempt.Jobs {
		if counts[job.Name] == 1 && job.Status == "completed" && job.Conclusion == "skipped" {
			result[job.Name] = true
		}
	}
	return result
}

func ciWorkflowFailureThroughSkips(runs []ActionsHeadRun, attempts []ActionsAttempt) (CIWorkflowFailure, bool, bool) {
	var eligible map[string]bool
	for index, run := range runs {
		var current ActionsAttempt
		matches := 0
		for _, attempt := range attempts {
			if matchingCIWorkflowAttempt(run, attempt) {
				current, matches = attempt, matches+1
			}
		}
		if matches != 1 || !validCIWorkflowJobs(run, current) || run.Event != runs[0].Event {
			return CIWorkflowFailure{}, false, false
		}
		counts := map[string]int{}
		for _, job := range current.Jobs {
			counts[job.Name]++
		}
		var failure CIWorkflowFailure
		for _, job := range current.Jobs {
			if ciFailedJob(job) && (index == 0 || eligible[job.Name] && counts[job.Name] == 1) && (failure.JobID == 0 || job.ID < failure.JobID) {
				failure = CIWorkflowFailure{WorkflowID: run.WorkflowID, RunID: run.ID, RunAttempt: run.RunAttempt, JobID: job.ID, CheckRunID: job.CheckRunID, Name: job.Name, Conclusion: job.Conclusion}
			}
		}
		if failure.JobID != 0 {
			return failure, true, false
		}
		skipped := ciSkippedWorkflowNames(run, current)
		if index == 0 {
			eligible = skipped
		} else {
			for name := range eligible {
				if !skipped[name] {
					delete(eligible, name)
				}
			}
		}
		if len(eligible) == 0 {
			return CIWorkflowFailure{}, false, false
		}
	}
	return CIWorkflowFailure{}, false, len(eligible) != 0
}

func sameCIWorkflowChain(original, current ActionsHeadInventory, failure CIWorkflowFailure) bool {
	if original.RepositoryID != current.RepositoryID || !strings.EqualFold(original.Repository, current.Repository) || original.HeadSHA != current.HeadSHA {
		return false
	}
	if _, err := LatestActionsRuns(current); err != nil {
		return false
	}
	minimum := int64(0)
	for _, run := range original.Runs {
		if run.WorkflowID == failure.WorkflowID && run.ID == failure.RunID {
			minimum = run.RunNumber
		}
	}
	if minimum == 0 {
		return false
	}
	prefix := func(in ActionsHeadInventory) ActionsHeadInventory {
		out := in
		out.Runs = []ActionsHeadRun{}
		for _, run := range in.Runs {
			if run.WorkflowID == failure.WorkflowID && run.RunNumber >= minimum {
				out.Runs = append(out.Runs, run)
			}
		}
		return out
	}
	return sameCIActions(prefix(original), prefix(current))
}

// CIWorkflowFailureApplies validates optional current-attempt attention. An
// older run additionally needs a complete skipped-job bridge and final run recheck.
func CIWorkflowFailureApplies(observation CIObservation, failure CIWorkflowFailure) bool {
	if observation.Actions == nil || observation.Actions.RepositoryID != observation.Target.RepositoryID || !strings.EqualFold(observation.Actions.Repository, observation.Target.Repository) || observation.Actions.HeadSHA != observation.SelectedCommitSHA {
		return false
	}
	latest, err := LatestActionsRuns(*observation.Actions)
	if err != nil {
		return false
	}
	for _, run := range latest.Runs {
		if run.WorkflowID == failure.WorkflowID && run.ID == failure.RunID && run.RunAttempt == failure.RunAttempt {
			return run.RepositoryID == observation.Target.RepositoryID && run.HeadSHA == observation.SelectedCommitSHA
		}
	}
	if observation.WorkflowActions == nil || !sameCIWorkflowChain(*observation.Actions, *observation.WorkflowActions, failure) {
		return false
	}
	attempts := append(append([]ActionsAttempt(nil), observation.Attempts...), observation.WorkflowAttempts...)
	actual, proven, _ := ciWorkflowFailureThroughSkips(ciWorkflowRuns(*observation.Actions, failure.WorkflowID), attempts)
	return proven && actual == failure
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
	if !validCIWorkflowJobs(run, attempt) {
		return out, false
	}
	for _, job := range attempt.Jobs {
		if ciFailedJob(job) && (out.JobID == 0 || job.ID < out.JobID) {
			out = CIWorkflowFailure{WorkflowID: run.WorkflowID, RunID: run.ID, RunAttempt: run.RunAttempt, JobID: job.ID, CheckRunID: job.CheckRunID, Name: job.Name, Conclusion: job.Conclusion}
		}
	}
	return out, out.JobID > 0
}

func validCIWorkflowJobs(run ActionsHeadRun, attempt ActionsAttempt) bool {
	if !matchingCIWorkflowAttempt(run, attempt) || attempt.Jobs == nil || len(attempt.Jobs) > githubMaxItems {
		return false
	}
	jobs, checks := map[int64]bool{}, map[int64]bool{}
	for _, job := range attempt.Jobs {
		if job.ID <= 0 || job.CheckRunID <= 0 || jobs[job.ID] || checks[job.CheckRunID] || job.HeadSHA != run.HeadSHA || !validPlainText(job.Name, 255) || strings.TrimSpace(job.Name) == "" || !validCIStateFacts(job.Status, job.Conclusion, job.StartedAt, job.CompletedAt) {
			return false
		}
		jobs[job.ID], checks[job.CheckRunID] = true, true
	}
	return true
}
