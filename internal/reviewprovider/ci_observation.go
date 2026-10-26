package reviewprovider

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"time"
)

// CITarget is an explicit observed source assertion, not a discovery selector.
type CITarget struct {
	RepositoryID int64  `json:"repository_id"`
	PullNumber   int64  `json:"pull_number"`
	Repository   string `json:"repository"`
	HeadSHA      string `json:"head_sha"`
	BaseSHA      string `json:"base_sha"`
	BaseRef      string `json:"base_ref"`
}

// CIObservation retains collected facts, including an empty test-merge
// inventory when it justifies head fallback. Nil pointers mean not fetched.
// Even a satisfied observation is not merge readiness or an atomic provider
// snapshot; its final reads bound detectable movement during collection.
type CIObservation struct {
	Assessment        RequiredCIAssessment   `json:"assessment"`
	Target            CITarget               `json:"target"`
	SelectedCommitSHA string                 `json:"selected_commit_sha"`
	Selection         string                 `json:"selection"`
	MergeState        string                 `json:"merge_state"`
	State             string                 `json:"state"`
	Reason            string                 `json:"reason"`
	StartedAt         time.Time              `json:"started_at"`
	ObservedAt        time.Time              `json:"observed_at"`
	Authority         RequiredCheckAuthority `json:"authority"`
	Candidate         *CICandidate           `json:"candidate,omitempty"`
	Results           *CIResultInventory     `json:"results,omitempty"`
	Actions           *ActionsHeadInventory  `json:"actions,omitempty"`
	Attempts          []ActionsAttempt       `json:"attempts"`
	TestMergeResults  *CIResultInventory     `json:"test_merge_results,omitempty"`
}

// ObserveRequiredCI performs bounded read-only collection for one exact target.
// Required context/app identity determines workflow fanout. Result-count movement
// permits one same-commit recollection within CIResults' original budgets. No
// job carry-forward, optional workflow walk or external mutation is performed.
func (g *GitHub) ObserveRequiredCI(ctx context.Context, target CITarget) (out CIObservation, err error) {
	if ctx == nil || !validRepositorySelector(target.Repository) || target.RepositoryID <= 0 || !validPullNumber(target.PullNumber) || !lowerCISHA(target.HeadSHA) || !lowerCISHA(target.BaseSHA) || !validRequiredBranch(target.BaseRef) {
		return CIObservation{}, invalidSelector()
	}
	out = CIObservation{Target: target, State: "unproven", Reason: "ci_authority_unproven", StartedAt: time.Now().UTC()}
	stage := "authority"
	defer func() {
		out.ObservedAt = time.Now().UTC()
		if err != nil {
			err = &ciCollectionError{stage: stage, cause: err}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out.Authority, err = g.RequiredCheckAuthority(ctx, target.Repository, target.BaseRef)
	if err != nil || out.Authority.State != "complete" || out.Authority.Checks == nil {
		return out, err
	}
	if len(out.Authority.Checks) > 128 {
		out.Reason = "ci_required_context_capacity"
		return out, nil
	}
	stage = "candidate"
	candidate, err := g.PullCICandidate(ctx, target.Repository, target.RepositoryID, target.PullNumber, target.HeadSHA)
	out.Candidate, out.MergeState = &candidate, candidate.MergeState
	if err != nil {
		out.Reason = "ci_source_unavailable"
		return out, err
	}
	if !matchesCITarget(candidate, target) {
		out.Reason = "ci_target_unproven"
		// Keep a known candidate diagnostic visible in the compact result. A
		// mergeable PR can still expose a stale, unverified test merge.
		if candidate.State == "unproven" {
			switch candidate.Reason {
			case "ci_merge_identity_unproven", "ci_merge_pending", "ci_candidate_changed", "ci_target_changed":
				out.Reason = candidate.Reason
			}
		}
		return out, nil
	}
	if len(out.Authority.Checks) == 0 {
		// Absence is proven by complete authority, not fabricated result facts.
		assessment, assessErr := AssessRequiredCI(out.Authority, CIResultInventory{Repository: target.Repository, HeadSHA: target.HeadSHA}, ActionsHeadInventory{}, nil)
		if assessErr != nil {
			return out, assessErr
		}
		out.Assessment = assessment
		out.State, out.Reason = assessment.State, assessment.Reason
	} else {
		if candidate.MergeState == "verified" {
			stage = "merge_results"
			merge, readErr := g.CIResults(ctx, target.Repository, candidate.MergeSHA)
			if readErr != nil {
				out.Reason = "ci_merge_results_unavailable"
				return out, readErr
			}
			out.TestMergeResults = &merge
			if len(merge.Checks) != 0 || len(merge.Statuses) != 0 {
				out.SelectedCommitSHA, out.Selection, out.Results = candidate.MergeSHA, "test_merge", &merge
			}
		}
		if out.Results == nil {
			out.SelectedCommitSHA, out.Selection = target.HeadSHA, "head"
			stage = "head_results"
			results, readErr := g.CIResults(ctx, target.Repository, target.HeadSHA)
			if readErr != nil {
				out.Reason = "ci_results_unavailable"
				return out, readErr
			}
			out.Results = &results
		}
		stage = "actions"
		actions, readErr := g.ActionsRunsForHead(ctx, target.Repository, target.RepositoryID, out.SelectedCommitSHA)
		if readErr != nil {
			out.Reason = "ci_actions_unavailable"
			return out, readErr
		}
		out.Actions = &actions
		stage = "workflow_identity"
		latest, readErr := LatestActionsRuns(actions)
		if readErr != nil {
			return out, readErr
		}
		relevant := relevantCIWorkflows(out.Authority.Checks, *out.Results, actions)
		if len(relevant) > 16 {
			out.Reason = "ci_workflow_capacity"
			return out, nil
		}
		out.Attempts = []ActionsAttempt{}
		for _, run := range latest.Runs {
			if !relevant[run.WorkflowID] {
				continue
			}
			stage = "attempt"
			attempt, readErr := g.CurrentActionsAttempt(ctx, target.Repository, out.SelectedCommitSHA, run.CheckSuiteID)
			if readErr != nil {
				out.Reason = "ci_attempt_unavailable"
				return out, readErr
			}
			out.Attempts = append(out.Attempts, attempt)
		}
		stage = "assessment"
		assessment, assessErr := AssessRequiredCI(out.Authority, *out.Results, actions, out.Attempts)
		if assessErr != nil {
			return out, assessErr
		}
		out.Assessment = assessment
		out.State, out.Reason = assessment.State, assessment.Reason
	}
	if out.State != "satisfied" && out.State != "not_required" {
		return out, nil
	}
	// A green result is provisional until these independently bounded reads
	// still agree. Status order is semantic: the provider's first context wins.
	out.State, out.Reason = "unproven", "ci_observation_changed"
	stage = "authority_recheck"
	authority, err := g.RequiredCheckAuthority(ctx, target.Repository, target.BaseRef)
	if err != nil || !sameCIAuthority(out.Authority, authority) {
		return out, err
	}
	stage = "candidate_recheck"
	finalCandidate, err := g.PullCICandidate(ctx, target.Repository, target.RepositoryID, target.PullNumber, target.HeadSHA)
	if err != nil || !sameCICandidate(candidate, finalCandidate) || !matchesCITarget(finalCandidate, target) {
		return out, err
	}
	if out.Results != nil {
		stage = "results_recheck"
		results, readErr := g.CIResults(ctx, target.Repository, out.SelectedCommitSHA)
		if readErr != nil || !sameCIResults(*out.Results, results) {
			return out, readErr
		}
		stage = "actions_recheck"
		actions, readErr := g.ActionsRunsForHead(ctx, target.Repository, target.RepositoryID, out.SelectedCommitSHA)
		if readErr != nil || !sameCIActions(*out.Actions, actions) {
			return out, readErr
		}
		if out.TestMergeResults != nil && out.Selection == "head" {
			stage = "merge_results_recheck"
			merge, readErr := g.CIResults(ctx, target.Repository, candidate.MergeSHA)
			if readErr != nil || !sameCIResults(*out.TestMergeResults, merge) {
				return out, readErr
			}
		}
	}
	if ctx.Err() != nil {
		return out, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	out.State, out.Reason = "satisfied", ""
	if len(out.Authority.Checks) == 0 {
		out.State = "not_required"
	}
	return out, nil
}

func matchesCITarget(candidate CICandidate, target CITarget) bool {
	return candidate.State == "complete" && candidate.RepositoryID == target.RepositoryID && strings.EqualFold(candidate.Repository, target.Repository) && candidate.PullNumber == target.PullNumber && candidate.HeadSHA == target.HeadSHA && candidate.BaseSHA == target.BaseSHA && candidate.BaseRef == target.BaseRef
}

func relevantCIWorkflows(required []RequiredCheck, results CIResultInventory, actions ActionsHeadInventory) map[int64]bool {
	suites := make(map[int64]int64, len(actions.Runs))
	for _, run := range actions.Runs {
		suites[run.CheckSuiteID] = run.WorkflowID
	}
	relevant := map[int64]bool{}
	for _, check := range results.Checks {
		for _, need := range required {
			if check.Name == need.Context && (need.AppID == 0 || check.AppID == need.AppID) && suites[check.SuiteID] > 0 {
				relevant[suites[check.SuiteID]] = true
			}
		}
	}
	return relevant
}

func sameCIAuthority(a, b RequiredCheckAuthority) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}
func sameCICandidate(a, b CICandidate) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}
func sameCIResults(a, b CIResultInventory) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	a.Checks, b.Checks = slices.Clone(a.Checks), slices.Clone(b.Checks)
	slices.SortFunc(a.Checks, func(a, b CICheckRun) int { return compareCIID(a.ID, b.ID) })
	slices.SortFunc(b.Checks, func(a, b CICheckRun) int { return compareCIID(a.ID, b.ID) })
	return reflect.DeepEqual(a, b)
}
func sameCIActions(a, b ActionsHeadInventory) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	a.Runs, b.Runs = slices.Clone(a.Runs), slices.Clone(b.Runs)
	slices.SortFunc(a.Runs, func(a, b ActionsHeadRun) int { return compareCIID(a.ID, b.ID) })
	slices.SortFunc(b.Runs, func(a, b ActionsHeadRun) int { return compareCIID(a.ID, b.ID) })
	return reflect.DeepEqual(a, b)
}
func compareCIID(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
