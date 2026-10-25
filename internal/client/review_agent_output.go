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

// Scheduled observation owns this issued Hosted trigger. Preserve uncertainty
// and every original identity; the caller need not escalate an ordinary gap.
func AgentReviewOperationStatus(value ReviewOperationStatus) ReviewOperationStatusOutput {
	if value.Channel == "hosted" && value.State == "uncertain" && value.Stage == "uncertain" && value.Reason == "hosted_observation_pending" && value.AttemptID != "" && value.NextAction == "contact_controller_owner" {
		value.NextAction = "wait_for_observation"
	}
	return ReviewOperationStatusOutput{value, agentReviewOperationDiagnostic(value.State, value.Reason)}
}

type ReviewOperationDiagnostic struct {
	Explanation string `json:"explanation"`
	NextAction  string `json:"next_action"`
}

type ReviewOperationStatusOutput struct {
	ReviewOperationStatus
	Diagnostic *ReviewOperationDiagnostic `json:"diagnostic,omitempty"`
}

type ReviewOperationOutput struct {
	ReviewOperation
	Diagnostic *ReviewOperationDiagnostic `json:"diagnostic,omitempty"`
}

func AgentReviewOperation(value ReviewOperation) ReviewOperationOutput {
	return ReviewOperationOutput{value, agentReviewOperationDiagnostic(value.State, value.Reason)}
}

// Explain retained admission facts without fetching source or authorizing a retry.
// Legacy parent_moved does not identify whether the selected or an ancestor edge
// failed; only the distinct predecessor reason proves the selected check passed.
func agentReviewOperationDiagnostic(state, reason string) *ReviewOperationDiagnostic {
	if state != "held" {
		return nil
	}
	var explanation string
	switch reason {
	case "parent_moved":
		explanation = "A checked PR's base and its direct parent branch differed. Older operations may report a selected PR or ancestor mismatch with this same reason; it does not identify the affected comparison."
	case "predecessor_parent_moved":
		explanation = "The selected PR's source check passed, but a parent/base mismatch was found while checking an ancestor."
	default:
		return nil
	}
	return &ReviewOperationDiagnostic{explanation, "Inspect the selected and ancestor comparisons and align the mismatched branch in its owning workflow. Use existing allow-out-of-order only for a verified stable selected comparison under the current review authorization; do not automatically retry, reset review progress or create a no-op commit."}
}

type ReviewCIDiagnostic struct {
	Explanation string `json:"explanation"`
	NextAction  string `json:"next_action"`
}
type ReviewCIOutput struct {
	ReviewSourceCIAssessment
	Diagnostic *ReviewCIDiagnostic `json:"diagnostic,omitempty"`
}

type ReviewReadinessOutput struct {
	ReviewSourceReadinessAssessment
	CIDiagnostic *ReviewCIDiagnostic `json:"ci_diagnostic,omitempty"`
}

func AgentReviewReadiness(value ReviewSourceReadinessAssessment) ReviewReadinessOutput {
	return ReviewReadinessOutput{value, agentCISummaryDiagnostic(value.CI)}
}

type ReviewSourceOperationOutput struct {
	ReviewSourceOperation
	CIDiagnostic *ReviewCIDiagnostic `json:"ci_diagnostic,omitempty"`
}

func AgentReviewSourceOperation(value ReviewSourceOperation) ReviewSourceOperationOutput {
	out := ReviewSourceOperationOutput{ReviewSourceOperation: value}
	if value.CI != nil {
		out.CIDiagnostic = agentCISummaryDiagnostic(*value.CI)
	}
	return out
}

// Preserve flat receipts and the original invalid-result checks. Only a retained
// operation can gain a CI hint; an unassessed pending receipt cannot.
func AgentReviewSourceResult(value ReviewSourceResult) any {
	if value.Operation != nil && value.Receipt == nil {
		return AgentReviewSourceOperation(*value.Operation)
	}
	return value
}

func agentCISummaryDiagnostic(value ReviewSourceCISummary) *ReviewCIDiagnostic {
	return AgentReviewCI(ReviewSourceCIAssessment{State: value.State, Reason: value.Reason}).Diagnostic
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
