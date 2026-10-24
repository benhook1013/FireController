package client

// ReviewRunsOutput omits workspace-origin metadata that does not date a run.
// Every original item, its own timestamps, and the pinned continuation survive.
type ReviewRunsOutput struct {
	ProjectID  string             `json:"project_id"`
	Revision   int64              `json:"revision"`
	SnapshotID string             `json:"snapshot_id"`
	Items      []ReviewRunSummary `json:"items"`
	Cursor     string             `json:"cursor,omitempty"`
	HasMore    bool               `json:"has_more"`
}

func AgentReviewRuns(value ReviewRuns) ReviewRunsOutput {
	return ReviewRunsOutput{value.ProjectID, value.Revision, value.SnapshotID, value.Items, value.Cursor, value.HasMore}
}

type ReviewCIDiagnostic struct {
	Explanation string `json:"explanation"`
	NextAction  string `json:"next_action"`
}
type ReviewCIOutput struct {
	ReviewSourceCIAssessment
	Diagnostic *ReviewCIDiagnostic `json:"diagnostic,omitempty"`
}

// AgentReviewCI explains the retained refusal; it neither refreshes nor credits CI.
func AgentReviewCI(value ReviewSourceCIAssessment) ReviewCIOutput {
	out := ReviewCIOutput{ReviewSourceCIAssessment: value}
	if value.State != "unproven" {
		return out
	}
	switch value.Reason {
	case "ci_merge_identity_unproven":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's test-merge identity could not be verified against this PR's observed head and base. Existing check results are not proof for that comparison.", "Check the current PR head/base and wait for GitHub to publish the matching test merge; then collect CI for that comparison. Do not create a no-op commit or rerun checks solely to bypass this refusal."}
	case "ci_provider_observation_changed_assessment", "ci_observation_changed":
		out.Diagnostic = &ReviewCIDiagnostic{"CI workflow state changed between provider reads, so this collection remains unproven. Zero retained check counts do not establish that no checks are required.", "Allow the current jobs to settle, then collect CI for the same comparison. Do not rerun workflows or reviews, reset allowances or change credentials to bypass this observation."}
	case "ci_provider_invalid_response_assessment":
		out.Diagnostic = &ReviewCIDiagnostic{"The collected CI facts could not be assessed consistently. This is unproven collection, not pending or passing CI; zero counts do not establish that no checks are required.", "Check the current CI and collect a fresh observation for the same comparison. If the rejection repeats, report the original refresh ID to the controller owner. Do not rerun workflows or reviews or change credentials solely for this refusal."}
	case "ci_merge_pending":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub has not yet published a verified test-merge candidate for this observed comparison.", "Wait for the candidate to become available, then collect CI for the same comparison. No review reset is needed."}
	}
	return out
}
