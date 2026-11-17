package reviewprovider

import "context"

// collectCIBody collects the existing per-target inventories and provisional
// assessment. It neither finalizes green proof nor changes the read order.
func (g *GitHub) collectCIBody(ctx context.Context, candidate CICandidate, out *CIObservation, stage *string) error {
	return g.collectCIBodyWithResults(ctx, candidate, out, stage, g.CIResults)
}

func (g *GitHub) collectCIBodyWithResults(ctx context.Context, candidate CICandidate, out *CIObservation, stage *string, read ciInventoryReader) error {
	target := out.Target
	if len(out.Authority.Checks) == 0 {
		// Absence is proven by complete authority, not fabricated result facts.
		assessment, assessErr := AssessRequiredCI(out.Authority, CIResultInventory{Repository: target.Repository, HeadSHA: target.HeadSHA}, ActionsHeadInventory{}, nil)
		if assessErr != nil {
			return assessErr
		}
		out.Assessment = assessment
		out.State, out.Reason = assessment.State, assessment.Reason
	} else {
		if candidate.MergeState == "verified" {
			*stage = "merge_results"
			merge, readErr := read(ctx, target.Repository, candidate.MergeSHA)
			if readErr != nil {
				out.Reason = "ci_merge_results_unavailable"
				return readErr
			}
			out.TestMergeResults = &merge
			if len(merge.Checks) != 0 || len(merge.Statuses) != 0 {
				out.SelectedCommitSHA, out.Selection, out.Results = candidate.MergeSHA, "test_merge", &merge
			}
		}
		if out.Results == nil {
			out.SelectedCommitSHA, out.Selection = target.HeadSHA, "head"
			*stage = "head_results"
			results, readErr := read(ctx, target.Repository, target.HeadSHA)
			if readErr != nil {
				out.Reason = "ci_results_unavailable"
				return readErr
			}
			out.Results = &results
		}
		*stage = "actions"
		actions, readErr := g.ActionsRunsForHead(ctx, target.Repository, target.RepositoryID, out.SelectedCommitSHA)
		if readErr != nil {
			out.Reason = "ci_actions_unavailable"
			return readErr
		}
		out.Actions = &actions
		*stage = "workflow_identity"
		latest, readErr := LatestActionsRuns(actions)
		if readErr != nil {
			return readErr
		}
		relevant := relevantCIWorkflows(out.Authority.Checks, *out.Results, actions)
		if len(relevant) > 16 {
			out.Reason = "ci_workflow_capacity"
			return nil
		}
		out.Attempts = []ActionsAttempt{}
		for _, run := range latest.Runs {
			if !relevant[run.WorkflowID] {
				continue
			}
			*stage = "attempt"
			// Only seed an exact attempt from this complete fresh head inventory.
			// The mandatory final direct run read rejects stale or moved identity.
			var discovered *ActionsHeadRun
			matches := 0
			for i := range actions.Runs {
				if actions.Runs[i].CheckSuiteID == run.CheckSuiteID {
					matches++
					discovered = &actions.Runs[i]
				}
			}
			if matches != 1 {
				discovered = nil
			}
			attempt, readErr := g.collectActionsAttempt(ctx, target.Repository, out.SelectedCommitSHA, run.CheckSuiteID, discovered, true)
			if readErr != nil {
				out.Reason = "ci_attempt_unavailable"
				return readErr
			}
			out.Attempts = append(out.Attempts, attempt)
		}
		*stage = "assessment"
		assessment, assessErr := AssessRequiredCI(out.Authority, *out.Results, actions, out.Attempts)
		if assessErr != nil {
			return assessErr
		}
		out.Assessment = assessment
		out.State, out.Reason = assessment.State, assessment.Reason
	}
	return nil
}

// finishCIProof retains every original final witness, in its original order.
// It is separate so a future cohort can coordinate candidate phases without
// treating provisional assessment as publishable green evidence.
func (g *GitHub) finishCIProof(ctx context.Context, gateRef string, candidate CICandidate, commits map[string]githubCIMerge, out *CIObservation, stage *string) error {
	target := out.Target
	if out.State != "satisfied" && out.State != "not_required" {
		return nil
	}
	// A green result is provisional until these independently bounded reads
	// still agree. Status order is semantic: the provider's first context wins.
	out.State, out.Reason = "unproven", "ci_observation_changed"
	*stage = "authority_recheck"
	authority, err := g.RequiredCheckAuthority(ctx, target.Repository, gateRef)
	if err != nil || !sameCIAuthority(out.Authority, authority) {
		return err
	}
	*stage = "candidate_recheck"
	finalCandidate, err := g.pullCICandidate(ctx, target.Repository, target.RepositoryID, target.PullNumber, target.HeadSHA, commits)
	if err != nil || !sameCICandidate(candidate, finalCandidate) || !matchesCITarget(finalCandidate, target) && !NonstrictHeadCICandidate(finalCandidate, target, out.Authority) {
		return err
	}
	return g.finishCIResults(ctx, candidate, out, stage)
}

// finishCIResults runs only after final authority and candidate agreement.
func (g *GitHub) finishCIResults(ctx context.Context, candidate CICandidate, out *CIObservation, stage *string) error {
	return g.finishCIResultsWithReader(ctx, candidate, out, stage, g.CIResults)
}

func (g *GitHub) finishCIResultsWithReader(ctx context.Context, candidate CICandidate, out *CIObservation, stage *string, read ciInventoryReader) error {
	target := out.Target
	if out.Results != nil {
		*stage = "results_recheck"
		results, readErr := read(ctx, target.Repository, out.SelectedCommitSHA)
		if readErr != nil || !sameCIResults(*out.Results, results) {
			return readErr
		}
		*stage = "actions_recheck"
		actions, readErr := g.ActionsRunsForHead(ctx, target.Repository, target.RepositoryID, out.SelectedCommitSHA)
		if readErr != nil || !sameCIActions(*out.Actions, actions) {
			return readErr
		}
		if out.TestMergeResults != nil && out.Selection == "head" {
			*stage = "merge_results_recheck"
			merge, readErr := read(ctx, target.Repository, candidate.MergeSHA)
			if readErr != nil || !sameCIResults(*out.TestMergeResults, merge) {
				return readErr
			}
		}
	}
	if ctx.Err() != nil {
		return providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	out.State, out.Reason = "satisfied", ""
	if len(out.Authority.Checks) == 0 {
		out.State = "not_required"
	}
	return nil
}
