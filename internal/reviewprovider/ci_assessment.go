package reviewprovider

import (
	"net/http"
	"reflect"
	"strings"
	"time"
)

// RequiredCIAssessment evaluates only the explicitly selected immutable commit.
// Even satisfied or not_required is neither a merge-readiness nor a freshness
// assertion; collecting the correct candidate and authority remains caller-owned.
type RequiredCIAssessment struct {
	Repository string                      `json:"repository"`
	HeadSHA    string                      `json:"head_sha"`
	State      string                      `json:"state"`
	Reason     string                      `json:"reason"`
	Checks     []RequiredCIAssessmentCheck `json:"checks"`
}

type RequiredCIAssessmentCheck struct {
	Context    string `json:"context"`
	AppID      int64  `json:"app_id"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	CheckRunID int64  `json:"check_run_id,omitempty"`
	StatusID   int64  `json:"status_id,omitempty"`
	RunID      int64  `json:"run_id,omitempty"`
	RunAttempt int64  `json:"run_attempt,omitempty"`
}

type ciAssessmentFacts struct {
	latest   map[int64]ActionsHeadRun
	suites   map[int64]ActionsHeadRun
	attempts map[int64]ActionsAttempt
	jobs     map[int64]map[int64]ActionsJob
	checks   map[string][]CICheckRun
	statuses map[string]CICommitStatus
}

// AssessRequiredCI is pure: it neither fetches nor retries any provider work.
// Status ordering is retained provider order, never reconstructed from clocks or
// IDs. Actions job identity comes only from an exact current numbered attempt.
func AssessRequiredCI(authority RequiredCheckAuthority, inventory CIResultInventory, actions ActionsHeadInventory, attempts []ActionsAttempt) (RequiredCIAssessment, error) {
	return assessRequiredCI(authority, inventory, actions, attempts, false)
}

// ValidRetainedRequiredCIAssessment accepts exact current facts, or the exact
// older conservative App-bound status-origin hold. It never upgrades retained
// results or accepts an arbitrary unknown assessment in place of source proof.
func ValidRetainedRequiredCIAssessment(authority RequiredCheckAuthority, inventory CIResultInventory, actions ActionsHeadInventory, attempts []ActionsAttempt, retained RequiredCIAssessment) bool {
	current, err := AssessRequiredCI(authority, inventory, actions, attempts)
	if err != nil {
		return false
	}
	if reflect.DeepEqual(current, retained) {
		return true
	}
	if retained.State != "unproven" || retained.Reason != "ci_required_checks_unproven" {
		return false
	}
	originHold := false
	for _, check := range retained.Checks {
		originHold = originHold || check.AppID > 0 && check.StatusID > 0 && check.State == "unproven" && check.Reason == "ci_status_origin_unproven"
	}
	if !originHold {
		return false
	}
	legacy, err := assessRequiredCI(authority, inventory, actions, attempts, true)
	return err == nil && reflect.DeepEqual(legacy, retained)
}

func assessRequiredCI(authority RequiredCheckAuthority, inventory CIResultInventory, actions ActionsHeadInventory, attempts []ActionsAttempt, legacyStatusOrigin bool) (RequiredCIAssessment, error) {
	result := RequiredCIAssessment{State: "unproven", Reason: "ci_authority_unproven", Checks: []RequiredCIAssessmentCheck{}}
	bad := func() (RequiredCIAssessment, error) {
		return RequiredCIAssessment{State: "unproven", Reason: "ci_facts_invalid", Checks: []RequiredCIAssessmentCheck{}}, invalidResponse(false, http.StatusOK)
	}
	if inventory.Repository != "" || inventory.HeadSHA != "" {
		if !validRepositorySelector(inventory.Repository) || !validSHA(inventory.HeadSHA) || inventory.HeadSHA != strings.ToLower(inventory.HeadSHA) {
			return bad()
		}
		result.Repository, result.HeadSHA = inventory.Repository, inventory.HeadSHA
	}
	if authority.State != "complete" {
		return result, nil
	}
	if !validRepositorySelector(authority.Repository) || !validRequiredBranch(authority.Branch) || authority.ObservedAt.IsZero() || authority.Reason != "" {
		return bad()
	}
	if authority.Checks == nil {
		return result, nil
	}
	if len(authority.Checks) > githubMaxItems {
		return bad()
	}
	required := map[RequiredCheck]bool{}
	apps := map[string]int64{}
	for _, check := range authority.Checks {
		if !validRequiredContext(check.Context) || check.AppID < 0 || required[check] {
			return bad()
		}
		if check.AppID > 0 {
			if prior := apps[check.Context]; prior != 0 && prior != check.AppID {
				return result, nil
			}
			apps[check.Context] = check.AppID
		}
		required[check] = true
	}
	if !validRepositorySelector(inventory.Repository) || !validSHA(inventory.HeadSHA) || inventory.HeadSHA != strings.ToLower(inventory.HeadSHA) || !strings.EqualFold(authority.Repository, inventory.Repository) {
		return bad()
	}
	// No requirements is an authority fact, not an observation of CI success.
	if len(authority.Checks) == 0 {
		result.State, result.Reason = "not_required", ""
		return result, nil
	}
	if inventory.ObservedAt.IsZero() || inventory.Checks == nil || inventory.Statuses == nil || actions.Runs == nil || attempts == nil {
		result.Reason = "ci_inventory_unproven"
		return result, nil
	}
	if !strings.EqualFold(actions.Repository, inventory.Repository) || actions.HeadSHA != inventory.HeadSHA {
		return bad()
	}
	latest, err := LatestActionsRuns(actions)
	if err != nil {
		return result, err
	}
	facts, err := assessmentFacts(inventory, actions, latest, attempts, len(required))
	if err != nil {
		return result, err
	}
	result.State, result.Reason = "satisfied", ""
	for _, required := range authority.Checks {
		entry := assessRequiredEntry(required, facts, legacyStatusOrigin)
		result.Checks = append(result.Checks, entry)
		if ciAssessmentRank(entry.State) > ciAssessmentRank(result.State) {
			result.State, result.Reason = entry.State, "ci_required_checks_"+entry.State
		}
	}
	return result, nil
}

func assessmentFacts(inventory CIResultInventory, actions, latest ActionsHeadInventory, attempts []ActionsAttempt, required int) (ciAssessmentFacts, error) {
	facts := ciAssessmentFacts{latest: map[int64]ActionsHeadRun{}, suites: map[int64]ActionsHeadRun{}, attempts: map[int64]ActionsAttempt{}, jobs: map[int64]map[int64]ActionsJob{}, checks: map[string][]CICheckRun{}, statuses: map[string]CICommitStatus{}}
	bad := func() (ciAssessmentFacts, error) { return ciAssessmentFacts{}, invalidResponse(false, http.StatusOK) }
	items := required + len(inventory.Checks) + len(inventory.Statuses) + len(actions.Runs) + len(attempts)
	if items > githubMaxItems {
		return bad()
	}
	for _, run := range latest.Runs {
		facts.latest[run.WorkflowID] = run
	}
	for _, run := range actions.Runs {
		facts.suites[run.CheckSuiteID] = run
	}
	checkIDs, statusIDs := map[int64]bool{}, map[int64]bool{}
	for _, check := range inventory.Checks {
		if check.ID <= 0 || check.SuiteID <= 0 || check.AppID <= 0 || checkIDs[check.ID] || check.HeadSHA != inventory.HeadSHA || !validRequiredContext(check.Name) || !validCIStateFacts(check.Status, check.Conclusion, check.StartedAt, check.CompletedAt) {
			return bad()
		}
		checkIDs[check.ID] = true
		facts.checks[check.Name] = append(facts.checks[check.Name], check)
	}
	for _, status := range inventory.Statuses {
		if status.ID <= 0 || statusIDs[status.ID] || !validRequiredContext(status.Context) || status.CreatedAt.IsZero() || status.UpdatedAt.IsZero() || status.UpdatedAt.Before(status.CreatedAt) || !validIdentifierText(status.State, 64) {
			return bad()
		}
		statusIDs[status.ID] = true
		if _, seen := facts.statuses[status.Context]; !seen {
			facts.statuses[status.Context] = status
		}
	}
	jobIDs, jobCheckIDs := map[int64]bool{}, map[int64]bool{}
	for _, attempt := range attempts {
		if !strings.EqualFold(attempt.Repository, inventory.Repository) || attempt.HeadSHA != inventory.HeadSHA || attempt.CheckSuiteID <= 0 || attempt.ObservedAt.IsZero() || (attempt.State != "complete" && attempt.State != "unproven") {
			return bad()
		}
		if _, duplicate := facts.attempts[attempt.CheckSuiteID]; duplicate {
			return bad()
		}
		facts.attempts[attempt.CheckSuiteID] = attempt
		if attempt.State != "complete" {
			if len(attempt.Jobs) != 0 {
				return bad()
			}
			continue
		}
		run, exists := facts.suites[attempt.CheckSuiteID]
		if !exists || attempt.RepositoryID != actions.RepositoryID || attempt.RunID != run.ID || attempt.WorkflowID != run.WorkflowID || attempt.RunNumber != run.RunNumber || attempt.Event != run.Event || attempt.RunAttempt != run.RunAttempt || !validIdentifierText(attempt.Status, 64) || attempt.Conclusion != "" && !validIdentifierText(attempt.Conclusion, 64) || attempt.Jobs == nil || attempt.Reason != "" {
			return bad()
		}
		items += len(attempt.Jobs)
		if items > githubMaxItems {
			return bad()
		}
		jobs := map[int64]ActionsJob{}
		for _, job := range attempt.Jobs {
			if job.ID <= 0 || job.CheckRunID <= 0 || jobIDs[job.ID] || jobCheckIDs[job.CheckRunID] || job.HeadSHA != inventory.HeadSHA || !validPlainText(job.Name, 255) || strings.TrimSpace(job.Name) == "" || !validCIStateFacts(job.Status, job.Conclusion, job.StartedAt, job.CompletedAt) {
				return bad()
			}
			jobIDs[job.ID], jobCheckIDs[job.CheckRunID] = true, true
			jobs[job.CheckRunID] = job
		}
		// Separate calls may observe one immutable attempt before and after a
		// lifecycle transition. Keep that observation unproven, but distinguish
		// movement from malformed facts or a different workflow/attempt identity.
		if attempt.Status != run.Status || attempt.Conclusion != run.Conclusion {
			return ciAssessmentFacts{}, &Error{Code: "provider_observation_changed", Message: "CI workflow state changed during observation.", Status: http.StatusOK}
		}
		facts.jobs[attempt.CheckSuiteID] = jobs
	}
	return facts, nil
}

// Skipped checks do not execute. GitHub can return a synthetic start after
// their completion; preserve those timestamps without treating them as duration
// or current-attempt evidence. Actual execution intervals remain ordered.
func validCIInterval(status, conclusion string, start, end time.Time) bool {
	return start.IsZero() || end.IsZero() || !end.Before(start) || status == "completed" && conclusion == "skipped"
}

// Unknown provider lifecycle tokens remain unproven, but malformed temporal or
// textual facts cannot participate in an assessment at all.
func validCIStateFacts(status, conclusion string, start, end time.Time) bool {
	if !validIdentifierText(status, 64) || conclusion != "" && !validIdentifierText(conclusion, 64) || !validCIInterval(status, conclusion, start, end) {
		return false
	}
	if status == "completed" {
		return !end.IsZero()
	}
	if !end.IsZero() || status == "in_progress" && start.IsZero() {
		return false
	}
	return true
}

func assessRequiredEntry(required RequiredCheck, facts ciAssessmentFacts, legacyStatusOrigin bool) RequiredCIAssessmentCheck {
	out := RequiredCIAssessmentCheck{Context: required.Context, AppID: required.AppID, State: "satisfied"}
	count := 0
	supersededWorkflows, currentChecks := map[int64]bool{}, map[int64]bool{}
	// A rerun keeps completed checks from earlier attempts in the same suite.
	// Only an exact current-attempt job/check match for this context and App
	// can exclude those retained terminal checks. Missing or ambiguous current
	// evidence never inherits an earlier attempt's result.
	currentAttemptScopes := map[[2]int64]bool{}
	for _, check := range facts.checks[required.Context] {
		if required.AppID != 0 && check.AppID != required.AppID {
			continue
		}
		run, known := facts.suites[check.SuiteID]
		attempt, observed := facts.attempts[check.SuiteID]
		job, exists := facts.jobs[check.SuiteID][check.ID]
		if known && run.ID == facts.latest[run.WorkflowID].ID && run.RunAttempt > 1 && run.Event != "workflow_dispatch" && run.Event != "workflow_run" && observed && attempt.State == "complete" && exists && job.Status == check.Status && job.Conclusion == check.Conclusion {
			currentAttemptScopes[[2]int64{check.SuiteID, check.AppID}] = true
		}
	}
	for _, check := range facts.checks[required.Context] {
		if required.AppID != 0 && check.AppID != required.AppID {
			continue
		}
		run, known := facts.suites[check.SuiteID]
		// Only complete positive workflow identity can make an old check irrelevant.
		if known && run.ID != facts.latest[run.WorkflowID].ID {
			supersededWorkflows[run.WorkflowID] = true
			continue
		}
		if check.Status == "completed" && currentAttemptScopes[[2]int64{check.SuiteID, check.AppID}] {
			if _, current := facts.jobs[check.SuiteID][check.ID]; !current {
				continue
			}
		}
		// A legacy same-name status cannot replace a check positively known to
		// belong to a workflow whose latest run has not supplied its own check.
		// Track every current positive job match even when duplicate contexts
		// separately make the aggregate ambiguous.
		if known && run.Event != "workflow_dispatch" && run.Event != "workflow_run" {
			attempt, observed := facts.attempts[check.SuiteID]
			job, exists := facts.jobs[check.SuiteID][check.ID]
			if observed && attempt.State == "complete" && exists && job.Status == check.Status && job.Conclusion == check.Conclusion {
				currentChecks[run.WorkflowID] = true
			}
		}
		count++
		if count > 1 {
			out.CheckRunID, out.RunID, out.RunAttempt = 0, 0, 0
			out.State, out.Reason = "unproven", "ci_check_ambiguous"
			continue
		}
		out.CheckRunID = check.ID
		if !known {
			out.State, out.Reason = "unproven", "ci_check_origin_unproven"
			continue
		}
		out.RunID, out.RunAttempt = run.ID, run.RunAttempt
		if run.Event == "workflow_dispatch" || run.Event == "workflow_run" {
			out.State, out.Reason = "unproven", "ci_event_unsupported"
			continue
		}
		attempt, exists := facts.attempts[check.SuiteID]
		if !exists || attempt.State != "complete" {
			out.State, out.Reason = "unproven", "ci_attempt_unproven"
			continue
		}
		job, exists := facts.jobs[check.SuiteID][check.ID]
		if !exists {
			out.State, out.Reason = "unproven", "ci_current_job_missing"
			continue
		}
		if job.Status != check.Status || job.Conclusion != check.Conclusion {
			out.State, out.Reason = "unproven", "ci_observation_changed"
			continue
		}
		out.State, out.Reason = ciCheckOutcome(job.Status, job.Conclusion)
	}
	for workflow := range supersededWorkflows {
		if !currentChecks[workflow] {
			out.CheckRunID, out.RunID, out.RunAttempt = 0, 0, 0
			out.State, out.Reason = "unproven", "ci_current_check_missing"
			break
		}
	}
	// A commit status cannot identify the required App. Even a successful
	// status needs its own current App-bound check/workflow proof first.
	if !legacyStatusOrigin && required.AppID > 0 && count == 0 && out.State == "satisfied" {
		out.State, out.Reason = "unproven", "ci_required_check_missing"
	}
	status, hasStatus := facts.statuses[required.Context]
	if hasStatus {
		out.StatusID = status.ID
		statusState, reason := "unproven", "ci_status_origin_unproven"
		if !legacyStatusOrigin || required.AppID == 0 {
			// A same-name status is a separate barrier, never App identity.
			switch status.State {
			case "success":
				statusState, reason = "satisfied", ""
			case "error", "failure":
				statusState, reason = "failed", "ci_status_failed"
			case "pending":
				statusState, reason = "pending", "ci_status_pending"
			default:
				statusState, reason = "unproven", "ci_status_unknown"
			}
		}
		if ciAssessmentRank(statusState) > ciAssessmentRank(out.State) {
			out.State, out.Reason = statusState, reason
		}
	}
	if count == 0 && !hasStatus {
		out.State, out.Reason = "unproven", "ci_required_check_missing"
	}
	return out
}

func ciCheckOutcome(status, conclusion string) (string, string) {
	if status == "completed" {
		switch conclusion {
		case "success", "neutral", "skipped":
			return "satisfied", ""
		case "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
			return "failed", "ci_check_failed"
		default:
			return "unproven", "ci_check_unknown"
		}
	}
	switch status {
	case "queued", "in_progress", "waiting", "pending", "requested":
		if conclusion == "" {
			return "pending", "ci_check_pending"
		}
	}
	return "unproven", "ci_check_unknown"
}
func ciAssessmentRank(state string) int {
	switch state {
	case "unproven":
		return 3
	case "failed":
		return 2
	case "pending":
		return 1
	}
	return 0
}
