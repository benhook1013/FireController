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
	if value.Channel == "hosted" && value.State == "held" && value.Reason == "hosted_allowance_cooldown" {
		value.NextAction = "new_request_after_cooldown"
	}
	if value.Channel == "hosted" && value.State == "held" && value.Reason == "hosted_allowance_reply_pending" {
		value.NextAction = "new_request_to_recheck_allowance"
	}
	if value.Channel == "cli" && value.State == "held" && value.Reason == "allowance_exhausted" {
		value.NextAction = "retry_later_with_new_request"
	}
	return ReviewOperationStatusOutput{value, agentReviewOperationDiagnostic(value.Channel, value.State, value.Reason)}
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
	return ReviewOperationOutput{value, agentReviewOperationDiagnostic(value.Channel, value.State, value.Reason)}
}

// Keep receipts flat and invalid unions rejected by their original marshaler.
func AgentReviewRequestResult(value ReviewRequestResult) any {
	if value.Operation != nil && value.Receipt == nil {
		return AgentReviewOperation(*value.Operation)
	}
	return value
}

type ReviewSelectionOutput struct {
	ReviewRunState
	Operation *ReviewOperationOutput `json:"operation,omitempty"`
}

func AgentReviewSelection(value ReviewRunState) ReviewSelectionOutput {
	out := ReviewSelectionOutput{ReviewRunState: value}
	if value.Operation != nil {
		op := AgentReviewOperation(*value.Operation)
		out.Operation = &op
	}
	return out
}

func AgentReviewRunResult(value ReviewRunResult) any {
	if value.Result != nil && value.Receipt == nil {
		return AgentReviewSelection(*value.Result)
	}
	return value
}

// Explain retained operation facts without fetching source or authorizing a retry.
// Legacy parent_moved does not identify whether the selected or an ancestor edge
// failed; only the distinct predecessor reason proves the selected check passed.
func agentReviewOperationDiagnostic(channel, state, reason string) *ReviewOperationDiagnostic {
	if channel == "hosted" && state == "active" && (reason == "trigger_recovered" || reason == "trigger_recorded" || reason == "trigger_posted") {
		return &ReviewOperationDiagnostic{"The original Hosted review trigger was confirmed on GitHub. This proves delivery, not that CodeRabbit started or is still running. updated_at records a local state transition, not provider start or the last observation; pending observations need not change it.", "Keep the existing observer for this operation and use a bounded status wait while continuing other work. The service's driver/recovery path observes the same trigger without reposting. This read exposes no provider-start time, exact next-observation deadline or finish ETA. Follow stage and evidence_id as results arrive; do not request another review, reset controls or infer a stuck provider from this reason's age alone."}
	}
	if state != "held" {
		return nil
	}
	var explanation string
	switch reason {
	case "allowance_exhausted":
		if channel != "cli" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The provider's included CLI allowance was observed at zero before review execution. This is provider quota, not this PR's local round limit; this hold completed no review. The held operation is terminal.", "Continue other work and avoid rapid retries. A later authorized new review_request with a fresh key and current configuration/head rechecks admission and provider allowance; the old key replays this hold. No reset timestamp or earliest retry is established. If retained, full_capacity_in_ms estimates a full allowance pool, not the next available slot; absent timing means unknown. Do not reset PR controls or infer availability from unused local rounds."}
	case "hosted_allowance_cooldown":
		if channel != "hosted" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The provider reported a cooldown before a review trigger was posted. This held operation is terminal: replaying its key returns the same result, and there is no resume command for this hold.", "Respect the retained retry_not_before deadline; if absent, verify the provider's cooldown reply first. Once eligible and still authorized, submit one new review_request with a fresh idempotency key, current configuration revision and qualified head. Normal admission and provider limits still apply. Preserve this held operation; do not reset controls or use a fresh key for an unknown outcome."}
	case "hosted_allowance_reply_pending":
		if channel != "hosted" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The original non-review rate-limit query was observed without an attributable reply during bounded preparation. No review trigger was posted. This hold is terminal: a late reply does not resume it.", "For the still-authorized planned review, use one later new review_request with a fresh idempotency key and current configuration/head. Normal preflight obtains fresh allowance and rechecks admission; no manual GitHub repair or extra round grant is required merely because preparation held. Respect any known provider cooldown and avoid rapid retries. An old available reply is not fresh permission. The original key replays this hold; do not repost its query or reset controls. An unknown action outcome must keep its original key instead."}
	case "hosted_allowance_unproven":
		return &ReviewOperationDiagnostic{"Hosted preparation could not establish included review availability from its retained query evidence. This is not proof of exhausted local review rounds or a started review.", "Inspect the original provider query/reply and report this operation and attempt to the controller maintainer if the cause remains unclear. Preserve the original request and key; do not reset round limits or blindly repost."}
	case "parent_moved":
		explanation = "A checked PR's base and its direct parent branch differed. Older operations may report a selected PR or ancestor mismatch with this same reason; it does not identify the affected comparison."
	case "predecessor_parent_moved":
		explanation = "The selected PR's source check passed, but a parent/base mismatch was found while checking an ancestor."
	default:
		return nil
	}
	return &ReviewOperationDiagnostic{explanation, "Inspect the selected and ancestor comparisons and align the mismatched branch in its owning workflow. Use the existing out-of-order request only for a verified stable selected comparison under the current review authorization; do not automatically retry, reset review progress or create a no-op commit."}
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
	case "ci_merge_base_mismatch":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's test merge contains this PR's head, but its other parent differs from the stable live base-branch tip. The PR endpoint's base.sha and merge_commit_sha can agree with each other while lagging a base-branch update. Checks on that merge do not prove the current-base comparison.", "Use the current base-branch comparison through the repository's normal update workflow, then collect its matching CI. Repeatedly reading the old refresh or rerunning checks on its old merge cannot establish that identity. Do not create a no-op commit, bypass CI or reset earned review/taper progress solely for this refusal."}
	case "ci_merge_identity_unproven":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's test-merge identity could not be verified against this PR's observed head and base. Existing check results are not proof for that comparison.", "Check the current PR head/base and wait for GitHub to publish the matching test merge; then collect CI for that comparison. Do not create a no-op commit or rerun checks solely to bypass this refusal."}
	case "ci_provider_observation_changed_assessment", "ci_observation_changed":
		out.Diagnostic = &ReviewCIDiagnostic{"CI workflow state changed between provider reads, so this collection remains unproven. Zero retained check counts do not establish that no checks are required.", "Allow the current jobs to settle, then collect CI for the same comparison. Do not rerun workflows or reviews, reset allowances or change credentials to bypass this observation."}
	case "ci_provider_observation_changed_head_results", "ci_provider_observation_changed_merge_results", "ci_provider_observation_changed_results_recheck", "ci_provider_observation_changed_merge_results_recheck":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's CI result counts kept changing during collection, including one bounded recollection. This observation remains unproven; zero counts do not mean that no checks are required.", "Routine background observation can capture the settled results. Do not rerun workflows or reviews, reset allowances or change credentials for this collection race."}
	case "ci_provider_invalid_response_head_results", "ci_provider_invalid_response_merge_results", "ci_provider_invalid_response_results_recheck", "ci_provider_invalid_response_merge_results_recheck":
		out.Diagnostic = &ReviewCIDiagnostic{"The check/status inventory for the selected commit could not be validated. CI is unproven; zero retained counts establish neither zero required checks nor a failed check.", "Reading or replaying this refresh preserves its as-of result. A later authorized collection can capture settled data; report repeated failures with its refresh ID. Do not rerun CI or change review controls to repair collection."}
	case "ci_provider_invalid_response_assessment":
		out.Diagnostic = &ReviewCIDiagnostic{"The collected CI facts could not be assessed consistently. This is unproven collection, not pending or passing CI; zero counts do not establish that no checks are required.", "Check the current CI and collect a fresh observation for the same comparison. If the rejection repeats, report the original refresh ID to the controller owner. Do not rerun workflows or reviews or change credentials solely for this refusal."}
	case "ci_merge_pending":
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub has not yet published a verified test-merge candidate for this observed comparison.", "Wait for the candidate to become available, then collect CI for the same comparison. No review reset is needed."}
	}
	return out
}
