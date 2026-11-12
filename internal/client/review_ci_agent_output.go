package client

import "fmt"

type ReviewCIDiagnostic struct {
	Explanation string `json:"explanation"`
	NextAction  string `json:"next_action"`
}
type ReviewCIOutput struct {
	ReviewSourceCIAssessment
	Diagnostic *ReviewCIDiagnostic `json:"diagnostic,omitempty"`
}

func AgentReviewCI(value ReviewSourceCIAssessment) ReviewCIOutput {
	out := ReviewCIOutput{ReviewSourceCIAssessment: value}
	if value.State == "pending" && value.Checks != nil {
		out.Diagnostic = pendingCommitStatusDiagnostic(*value.Checks)
		return out
	}
	if value.State != "unproven" {
		return out
	}
	switch value.Reason {
	case "ci_merge_base_mismatch":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's test merge contains this PR's head, but its other parent differs from the stable live base-branch tip. The PR endpoint's base.sha and merge_commit_sha can agree with each other while lagging a base-branch update. Identical old and current base trees do not establish the required parent identity. Checks on that merge do not prove the current-base comparison.", "First allow GitHub to regenerate the current test merge after a retarget or base update; a later source refresh can observe it. If the live base is stable and GitHub keeps the stale merge, reconcile the PR branch with that base through the repository's normal authorized merge/rebase workflow, then collect CI for the resulting current comparison. FireController source refresh observes comparisons; it does not regenerate GitHub merge refs or update branches. Repeatedly reading the old refresh or rerunning checks on its old merge cannot establish that identity. Do not create a no-op commit, bypass CI or reset earned review/taper progress solely for this refusal."}
		if facts := value.MergeBaseMismatch; facts != nil && validCIMergeBaseMismatch(facts, value.Target.HeadSHA, value.Target.BaseSHA) {
			out.Diagnostic.Explanation = fmt.Sprintf("Observed PR endpoint base.sha %s; independently read stable live base-branch tip %s; test merge %s has base parent %s and the PR head. These are distinct as-of provider facts. ", facts.PRBaseSHA, facts.LiveBaseSHA, facts.MergeSHA, facts.MergeBaseParentSHA) + out.Diagnostic.Explanation
		}
	case "ci_merge_identity_unproven":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's test-merge identity could not be verified against this PR's observed head and base. Existing check results are not proof for that comparison.", "Check the current PR head/base and wait for GitHub to publish the matching test merge; then collect CI for that comparison. Do not create a no-op commit or rerun checks solely to bypass this refusal."}
	case "ci_provider_observation_changed_assessment", "ci_observation_changed":
		out.Diagnostic = &ReviewCIDiagnostic{"CI workflow state changed between provider reads, so this collection remains unproven. Zero retained check counts do not establish that no checks are required.", "Allow the current jobs to settle, then collect CI for the same comparison. Do not rerun workflows or reviews, reset allowances or change credentials to bypass this observation."}
	case "ci_provider_observation_changed_head_results", "ci_provider_observation_changed_merge_results", "ci_provider_observation_changed_results_recheck", "ci_provider_observation_changed_merge_results_recheck":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's CI result counts kept changing during collection, including one bounded recollection. This observation remains unproven; zero counts do not mean that no checks are required.", "Routine background observation can capture the settled results. Do not rerun workflows or reviews, reset allowances or change credentials for this collection race."}
	case "ci_provider_inventory_incomplete", "ci_provider_inventory_incomplete_head_results", "ci_provider_inventory_incomplete_merge_results", "ci_provider_inventory_incomplete_results_recheck", "ci_provider_inventory_incomplete_merge_results_recheck":
		out.Diagnostic = &ReviewCIDiagnostic{"The complete check-suite/check-run inventory could not be established because provider counts were inconsistent or exceeded the supported inventory bound. This is an inventory-completeness refusal, not proof of a provider outage or an empty CI gate.", "A later bounded CI wait or source collection can observe a complete inventory; inspect the selected commit's inventory if the refusal persists. Preserve the unproven result. Do not rerun workflows or reviews, change credentials or reset grants solely for this refusal."}
	case "ci_provider_invalid_response_head_results", "ci_provider_invalid_response_merge_results", "ci_provider_invalid_response_results_recheck", "ci_provider_invalid_response_merge_results_recheck":
		out.Diagnostic = &ReviewCIDiagnostic{"The check/status inventory for the selected commit could not be validated. CI is unproven; zero retained counts establish neither zero required checks nor a failed check.", "Reading or replaying this refresh preserves its historical result. A later bounded CI wait or authorized source collection can observe settled valid facts for the same comparison. Do not rerun CI, adjust clocks or change review controls solely for this invalid observation."}
	case "ci_provider_invalid_response_check_run_time_order", "ci_provider_invalid_response_head_results_check_run_time_order", "ci_provider_invalid_response_merge_results_check_run_time_order", "ci_provider_invalid_response_results_recheck_check_run_time_order", "ci_provider_invalid_response_merge_results_recheck_check_run_time_order":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's check-runs endpoint returned completed_at earlier than started_at for a non-skipped check. These contradictory timestamps leave CI unproven; zero retained counts establish neither zero required checks nor a failed gate.", "Continue other work while normal observation checks for corrected provider metadata. A later bounded CI wait or authorized source collection can observe valid facts for the same comparison; this retained refresh remains historical. Do not assume a clock offset, adjust clocks, rerun CI or change review grants solely for this invalid observation."}
	case "ci_provider_invalid_response_assessment":
		out.Diagnostic = &ReviewCIDiagnostic{"The collected CI facts could not be assessed consistently. This is unproven collection, not pending or passing CI; zero counts do not establish that no checks are required.", "This retained refresh preserves its historical result. A later bounded CI wait or authorized source collection can observe settled valid facts for the same comparison. Do not rerun workflows or reviews, adjust clocks or change credentials or review grants solely for this refusal."}
	case "ci_merge_pending":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub has not yet published a verified test-merge candidate for this observed comparison.", "Wait for the candidate to become available, then collect CI for the same comparison. No review reset is needed."}
	}
	return out
}

// Explain one actual required-context status, preferring the paired successful
// check that can otherwise make its separate pending commit status confusing.
func pendingCommitStatusDiagnostic(checks []ReviewCIContext) *ReviewCIDiagnostic {
	var selected *ReviewCIContext
	for i := range checks {
		check := &checks[i]
		if check.State != "pending" || !controlText(check.Context, 255, true) || check.CommitStatus == nil || check.CommitStatus.ID < 1 || check.CommitStatus.State != "pending" {
			continue
		}
		if selected == nil {
			selected = check
		}
		if check.CheckRun != nil && check.CheckRun.ID > 0 && check.CheckRun.Status == "completed" && check.CheckRun.Conclusion == "success" {
			selected = check
			break
		}
	}
	if selected == nil {
		return nil
	}
	explanation := fmt.Sprintf("One required context, %q, has pending commit status %d in this observation; this is pending, not a CI failure.", selected.Context, selected.CommitStatus.ID)
	if check := selected.CheckRun; check != nil && check.ID > 0 {
		if check.Status == "completed" && check.Conclusion == "success" {
			explanation += fmt.Sprintf(" Its separate check run %d completed successfully; that does not complete the commit status.", check.ID)
		} else {
			explanation += fmt.Sprintf(" Its separate check run %d is also retained.", check.ID)
		}
	}
	return &ReviewCIDiagnostic{explanation, "Continue other work; do not rerun CI solely to clear this pending commit status. Inspect its posting source if it persists. These are status/check IDs, not an attributed workflow run."}
}
