package client

import "fmt"

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
	if value.Channel == "hosted" && value.State == "rate_limited" && value.Reason == "hosted_review_rate_limited" {
		value.NextAction = "retry_later_with_new_request"
	}
	if value.Channel == "cli" && value.State == "held" && (value.Reason == "allowance_exhausted" || value.Reason == "allowance_timeout" || value.Reason == "allowance_response_unproven") {
		value.NextAction = "retry_later_with_new_request"
	}
	hint := agentReviewOperationDiagnostic(value.Channel, value.State, value.Reason)
	if hostedActivityDiagnostic(value.Channel, value.State, value.Reason) != nil {
		value.NextAction = "inspect_provider_activity"
	}
	if value.Wait != nil {
		hint = operationWaitDiagnostic(value.Wait)
	}
	if value.State == "held" && value.Reason == "predecessor_unfinished" {
		hint = predecessorUnfinishedDiagnostic(value.SourceRefusal, value.Channel)
	}
	if value.State == "held" && value.Reason == "parent_unavailable" {
		hint = parentUnavailableDiagnostic(value.SourceRefusal)
	}
	if value.State == "held" && (value.Reason == "parent_moved" || value.Reason == "predecessor_parent_moved") && value.SourceRefusal != nil {
		hint = parentComparisonDiagnostic(value.SourceRefusal, value.Reason)
	}
	if value.PendingEvidence != nil {
		hint = pendingEvidenceDiagnostic(value.PendingEvidence)
		value.NextAction = "resolve_pending_evidence"
		if len(value.PendingEvidence.Items) == 0 {
			value.NextAction = "new_request_if_authorized"
		}
	}
	if value.State == "queued" && value.queuedCause != "" && (value.Wait == nil || !value.Wait.Paused) {
		hint = queuedOperationDiagnostic(value.queuedCause)
	}
	if value.State == "queued" && value.deploymentDraining {
		hint = deploymentDrainDiagnostic()
		value.NextAction = "wait_for_deployment"
	}
	return ReviewOperationStatusOutput{value, hint}
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
	hint := agentReviewOperationDiagnostic(value.Channel, value.State, value.Reason)
	if hint != nil && value.Channel == "hosted" && value.State == "held" && value.Reason == "hosted_allowance_unproven" {
		const laterRequest = "No review trigger was authorized by this terminal hold. If the planned review remains authorized, one later fresh request/key rechecks current source, configuration, rounds and provider allowance. Do not directly repost the old query, reset limits or assume permission."
		if value.HostedQuery == nil {
			hint = &ReviewOperationDiagnostic{hint.Explanation, "No original query handle is available in this response. " + laterRequest}
		} else if value.HostedQuery.QueryURL != "" {
			hint = &ReviewOperationDiagnostic{hint.Explanation, "Open hosted_query.query_url to inspect this operation's original non-review query and adjacent provider reply. hosted_query records original retained handles, not current availability. Preserve the original request/key; do not reset limits or repost the query."}
		} else {
			hint = &ReviewOperationDiagnostic{hint.Explanation, "The original non-review query intent is retained but its posting was not confirmed. " + laterRequest}
		}
	}
	if value.Wait != nil {
		hint = operationWaitDiagnostic(value.Wait)
	}
	if value.State == "held" && value.Reason == "predecessor_unfinished" {
		hint = predecessorUnfinishedDiagnostic(value.SourceRefusal, value.Channel)
	}
	if value.State == "held" && value.Reason == "parent_unavailable" {
		hint = parentUnavailableDiagnostic(value.SourceRefusal)
	}
	if value.State == "held" && (value.Reason == "parent_moved" || value.Reason == "predecessor_parent_moved") && value.SourceRefusal != nil {
		hint = parentComparisonDiagnostic(value.SourceRefusal, value.Reason)
	}
	if value.PendingEvidence != nil {
		hint = pendingEvidenceDiagnostic(value.PendingEvidence)
	}
	if value.State == "queued" && value.queuedCause != "" && (value.Wait == nil || !value.Wait.Paused) {
		hint = queuedOperationDiagnostic(value.queuedCause)
	}
	if value.State == "queued" && value.deploymentDraining {
		hint = deploymentDrainDiagnostic()
	}
	return ReviewOperationOutput{value, hint}
}

// Keep receipts flat and invalid unions rejected by their original marshaler.
func deploymentDrainDiagnostic() *ReviewOperationDiagnostic {
	return &ReviewOperationDiagnostic{"The controller is draining new dispatch for deployment. Stored request and review state are unchanged.", "Continue other work and collect this same operation later. The replacement resumes queued work under the normal checks; do not submit a duplicate request or change review controls."}
}

func AgentReviewRequestResult(value ReviewRequestResult) any {
	if value.Receipt != nil && value.Operation == nil && value.Receipt.deploymentDraining {
		return struct {
			*ReviewRequestReceipt
			Diagnostic *ReviewOperationDiagnostic `json:"diagnostic"`
		}{value.Receipt, deploymentDrainDiagnostic()}
	}
	if value.Receipt != nil && value.Operation == nil && value.Receipt.State == "queued" && value.queuedCause != "" {
		return struct {
			*ReviewRequestReceipt
			Diagnostic *ReviewOperationDiagnostic `json:"diagnostic"`
		}{value.Receipt, queuedOperationDiagnostic(value.queuedCause)}
	}
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
	if hint := hostedActivityDiagnostic(channel, state, reason); hint != nil {
		return hint
	}
	if channel == "hosted" && state == "rate_limited" && reason == "hosted_review_rate_limited" {
		return &ReviewOperationDiagnostic{"CodeRabbit rejected the original review trigger because its review rate limit was reached. This operation is terminal; no completed review or clean-round credit was recorded.", "Waiting on this operation will not start a review. Continue other work. If another review remains authorized, use one later fresh request/key with current configuration/head through normal allowance preflight. The rejection alone supplies no exact retry deadline. Do not repost this trigger, reset round controls or treat it as a completed review."}
	}
	if channel == "hosted" && state == "active" && (reason == "trigger_recovered" || reason == "trigger_recorded" || reason == "trigger_posted") {
		return &ReviewOperationDiagnostic{"The original Hosted review trigger was confirmed on GitHub. This proves delivery, not that CodeRabbit started or is still running. updated_at records a local state transition, not provider start or the last observation; pending observations need not change it.", "Keep the existing observer for this operation and use a bounded status wait while continuing other work. The service's driver/recovery path observes the same trigger without reposting. Use observer.last_check_at and observer.last_source_at for actual check freshness when available; observer.next_check_at is the current schedule, not an exact deadline or provider-start/finish ETA. Follow stage and evidence_id as results arrive; do not request another review, reset controls or infer a stuck provider from this reason's age alone."}
	}
	if state != "held" {
		return nil
	}
	var explanation string
	switch reason {
	case "review_evidence_pending":
		return &ReviewOperationDiagnostic{"Earlier native evidence in this PR/channel still needs capture, adjudication, finalization, fix proof or a routed receiving resolution. This held operation is terminal and completed no new review.", "Read this operation with diagnostics to identify the current source operation/evidence, finding and route handles. Resolve the actual retained obligation through its existing controls; an outgoing accepted route without fix proof can hold the source channel. Do not reset rounds, discard evidence or start another review to clear the hold."}
	case "project_file_limit":
		return &ReviewOperationDiagnostic{"The materialized comparison exceeds the optional project review-file ceiling. This operation is terminal and no review was started.", "Reduce the comparison, or deliberately select override_file_limit:true on a new authorized exact-target review_request (CLI --override-file-limit), with a fresh key and current configuration/head. This overrides only the project ceiling; provider restrictions, findings, allowances and all other admission checks still apply. Do not change project defaults or replay the held key expecting execution."}
	case "allowance_timeout":
		if channel != "cli" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The included-allowance command did not complete within its configured allowance-check deadline (15 seconds by default). No review command was started and no review completed. This held operation is terminal. Missing allowance facts establish neither exhausted quota nor a provider outage; the next available retry time is unknown.", "Continue other work and avoid rapid retries. If the planned review remains authorized, make one later review_request with a fresh idempotency key and current configuration/head to recheck allowance through normal admission. The original key replays this hold; waiting on it will not resume it. Do not reset round controls, infer availability from local rounds or manually rerun the review. Repeated timeouts can be reported with the original operation/attempt IDs."}
	case "allowance_response_unproven":
		if channel != "cli" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The CLI allowance command completed successfully, but its response could not be validated. No review command started during this preparation. This terminal hold does not establish a remaining balance, exhausted quota or a retry time.", "Continue other work and avoid rapid retries. If the planned review remains authorized, use one later review_request with a fresh idempotency key and current configuration/head for normal allowance checks. The original key replays this hold; waiting will not resume it. Do not reset controls or manually run the review. Report repeated failures with the original operation/attempt IDs."}
	case "allowance_exhausted":
		if channel != "cli" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The provider's included CLI allowance was observed at zero before review execution. This is provider quota, not this PR's local round limit; this hold completed no review. The held operation is terminal.", "Continue other work and avoid rapid retries. A later authorized new review_request with a fresh key and current configuration/head rechecks admission and provider allowance; the old key replays this hold. No reset timestamp or earliest retry is established. If retained, full_capacity_in_ms estimates a full allowance pool, not the next available slot; absent timing means unknown. Do not reset PR controls or infer availability from unused local rounds."}
	case "hosted_allowance_cooldown":
		if channel != "hosted" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The provider reported a cooldown before a review trigger was posted. This held operation is terminal: replaying its key returns the same result, and there is no resume command for this hold. It does not reserve the next shared provider slot or retain a waiting queue position; other authorized requests can consume that slot.", "Respect the retained retry_not_before deadline; if absent, verify the provider's cooldown reply first. This deadline is an earliest recheck time, not a reserved start time. Once eligible and still authorized, submit one new review_request with a fresh idempotency key, current configuration revision and qualified head. Coordinate competing requests under the project's intended priority. Normal admission and provider limits still apply. Preserve this held operation; do not reset controls or use a fresh key for an unknown outcome."}
	case "hosted_allowance_reply_pending":
		if channel != "hosted" {
			return nil
		}
		return &ReviewOperationDiagnostic{"The original non-review rate-limit query was observed without an attributable reply during bounded preparation. No review trigger was posted. This hold is terminal: a late reply does not resume it.", "For the still-authorized planned review, use one later new review_request with a fresh idempotency key and current configuration/head. Normal preflight obtains fresh allowance and rechecks admission; no manual GitHub repair or extra round grant is required merely because preparation held. Respect any known provider cooldown and avoid rapid retries. An old available reply is not fresh permission. The original key replays this hold; do not repost its query or reset controls. An unknown action outcome must keep its original key instead."}
	case "hosted_allowance_unproven":
		return &ReviewOperationDiagnostic{"Hosted preparation could not establish included review availability from its retained query evidence. This is not proof of exhausted local review rounds or a started review.", "Read this same operation with diagnostics:true (CLI --diagnostics) for its original hosted_query link and retained source handles when available. Preserve the original request/key; do not reset limits or blindly repost."}
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
		out.Diagnostic = &ReviewCIDiagnostic{"GitHub's test merge contains this PR's head, but its other parent differs from the stable live base-branch tip. The PR endpoint's base.sha and merge_commit_sha can agree with each other while lagging a base-branch update. Identical old and current base trees do not establish the required parent identity. Checks on that merge do not prove the current-base comparison.", "First allow GitHub to regenerate the current test merge after a retarget or base update; a later source refresh can observe it. If the live base is stable and GitHub keeps the stale merge, reconcile the PR branch with that base through the repository's normal authorized merge/rebase workflow, then collect CI for the resulting current comparison. FireController source refresh observes comparisons; it does not regenerate GitHub merge refs or update branches. Repeatedly reading the old refresh or rerunning checks on its old merge cannot establish that identity. Do not create a no-op commit, bypass CI or reset earned review/taper progress solely for this refusal."}
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

func pendingEvidenceDiagnostic(v *ReviewPendingEvidence) *ReviewOperationDiagnostic {
	if len(v.Items) == 0 {
		return &ReviewOperationDiagnostic{"No current native-evidence blockers remain for this PR/channel. This is a current local assessment; the earlier held operation and its original reason remain unchanged.", "If the planned review remains authorized, use a new request with a fresh key and current configuration/head. All other admission and provider checks still apply; this read grants no review permission."}
	}
	return &ReviewOperationDiagnostic{"The pending_evidence items name the current local obligations in this PR/channel, including any unresolved outgoing route and its receiving PR. evaluated_at describes this read, not a captured historical cause of the earlier hold.", "Resolve the listed capture, finding, finalization, fix or receiving-route obligation through its existing controls. Read the referenced evidence/adjudication or route only when more context is needed. has_more means this is a bounded preview. The held operation remains terminal; do not reset controls, discard evidence or request another review to repair missing work."}
}

func parentUnavailableDiagnostic(refusal *ReviewSourceRefusal) *ReviewOperationDiagnostic {
	explanation := "Preparation could not establish an open parent for a checked PR's base branch. This hold did not retain which comparison failed."
	if refusal != nil {
		explanation = fmt.Sprintf("Preparation could not establish an open parent for PR #%d's base branch %q. These are the original lookup facts, not a current source observation.", refusal.PullNumber, refusal.BaseRef)
	}
	return &ReviewOperationDiagnostic{explanation, "Inspect that base branch in its owning workflow; align or retarget it if its former parent merged. After the source is qualified, use a fresh key and current guards for the still-authorized review. No round reset or extra grant is required. Out-of-order requests still check the target's own parent."}
}

func operationWaitDiagnostic(w *ReviewOperationWait) *ReviewOperationDiagnostic {
	scope, candidates := "", ""
	if w.RetryNotBefore != "" {
		scope = " This deadline schedules this request only; the provider limit’s scope across other PRs, repositories or accounts is unknown."
		candidates = " Other authorized candidates still need their own admission and fresh provider checks."
	}
	if w.Paused {
		return &ReviewOperationDiagnostic{"This same operation is paused before provider dispatch. Its retry deadline and review intent are retained; no review is running." + scope, "Continue other work, or explicitly resume/cancel this operation with its current revision and an idempotency key. This does not change project pause, allowances or taper." + candidates}
	}
	if w.RetryNotBefore != "" {
		return &ReviewOperationDiagnostic{"The provider reported a cooldown. This same operation automatically rechecks admission and allowance at or after retry_not_before; the deadline is not a reserved start time or review completion." + scope, "Continue other work and read this operation later. Do not submit a replacement request. Pause or cancel only this undispatched intent using review_operation_control (CLI reviews operation-control) if desired." + candidates}
	}
	return nil
}

func predecessorUnfinishedDiagnostic(refusal *ReviewSourceRefusal, channel string) *ReviewOperationDiagnostic {
	explanation := "The selected review was held before provider dispatch by an ancestor dependency. The original blocking PR and specific cause were not retained; this does not identify unfinished findings on the selected PR."
	action := "Inspect the current scoped reviews queue and its ancestor chain. Current facts do not reconstruct the original hold."
	if refusal != nil {
		cause := "its native channel discovery was incomplete (pending evidence, incomplete configured rounds or missing coverage; not necessarily findings)"
		switch refusal.Cause {
		case "not_in_queue":
			cause = "it was absent from the configured execution queue"
		case "incoming_route_pending":
			cause = "it had an unresolved incoming receiving route"
		}
		explanation = fmt.Sprintf("The selected review was held before provider dispatch by predecessor PR #%d because %s. These are original retained guard facts, not a current observation or a finding on the selected PR.", refusal.PullNumber, cause)
		action = fmt.Sprintf("Read reviews pull %d and reviews rounds read --pr %d --channel %s for current scoped PR and channel progress; follow their retained evidence, finding and route handles when needed. For not_in_queue, also read reviews execution and inspect its configured queue. These reads perform no provider refresh.", refusal.PullNumber, refusal.PullNumber, channel)
	}
	return &ReviewOperationDiagnostic{explanation, action + " This held operation is terminal. Any still-authorized new request needs a fresh key/current guards. Explicit out-of-order selection only bypasses predecessor progression; the selected target's own source, evidence, policy and provider guards still apply. Do not automatically retry, reset rounds or invent historical facts."}
}

func parentComparisonDiagnostic(v *ReviewSourceRefusal, reason string) *ReviewOperationDiagnostic {
	checked := "selected PR"
	prefix := "Preparation checked "
	if reason == "predecessor_parent_moved" {
		checked = "ancestor PR"
		prefix = "The selected PR's source check passed; preparation checked "
	}
	explanation := fmt.Sprintf("%s%s #%d against parent PR #%d: original observed base %s differs from direct-parent head %s recorded at the check. These are retained facts, not a current observation.", prefix, checked, v.PullNumber, v.ParentPullNumber, v.ObservedBaseSHA, v.ExpectedParentSHA)
	action := fmt.Sprintf("Check current PR #%d, its live named base, direct parent PR #%d and ancestry. Align an actually mismatched link through its owning workflow. If the named base already matches the parent and the child includes it, GitHub PR metadata may be stale: the owning GitHub workflow may reapply that same existing base branch, then verify the returned head is unchanged and base matches the parent. Qualify the stable comparison afresh under current review authorization; this terminal held operation does not resume. Out-of-order selection still checks the selected PR's own source. This diagnostic performs no mutation or retry. Use existing workflow authorization; do not reset review progress or temporarily retarget, force-push or create a no-op commit merely to refresh metadata.", v.PullNumber, v.ParentPullNumber)
	return &ReviewOperationDiagnostic{explanation, action}
}
