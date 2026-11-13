package client

// These fields explain a read result after wire validation. The transport DTO,
// original source pins, check facts and observation timestamps stay unchanged.
type CIWatchTargetOutput struct {
	CIWatchTargetResult
	Phase      string                     `json:"phase,omitempty"`
	Diagnostic *ReviewOperationDiagnostic `json:"diagnostic,omitempty"`
}

type CIWatchOutput struct {
	CIWatchResult
	AllTargetsPassed bool                  `json:"all_targets_passed"`
	Targets          []CIWatchTargetOutput `json:"targets"`
}

func AgentCIWatch(value CIWatchResult) CIWatchOutput {
	out := CIWatchOutput{CIWatchResult: value, Targets: make([]CIWatchTargetOutput, 0, len(value.Targets)), AllTargetsPassed: len(value.Targets) > 0}
	for _, item := range value.Targets {
		out.AllTargetsPassed = out.AllTargetsPassed && item.State == "passed"
		phase, diagnostic := ciWatchDiagnostic(item)
		if diagnostic != nil && item.ObservationKey != "" && (item.WaitingForChange || len(item.WorkflowFailures) > 0) {
			copy := *diagnostic
			copy.NextAction += " Start another bounded CI wait if changed evidence is still needed; this response ended the subscription. Reuse observation_key as after_observation_key, retain the head pin and workflow selection, and send returned observed_base_sha as base_sha if the original target omitted base_sha."
			diagnostic = &copy
		}
		if len(item.WorkflowObservations) > 0 || len(item.WorkflowFailures) > 0 {
			limit := " Failure rows identify the exact run/current attempt with a proven failed execution; this may precede newer runs that explicitly skipped that job. Missing workflow details and absent failure rows do not certify that other jobs were checked."
			for _, row := range item.WorkflowFailures {
				if row.Basis == "historical_same_head_failure_supersession_unproven" {
					limit = " Failure rows identify real failed jobs in the supplied run/current numbered attempt. A historical row leaves latest-job coverage or supersession unproven; unlabelled earlier rows require the proven skipped-job chain. Missing workflow details and absent failure rows do not certify that other jobs were checked."
					break
				}
			}
			if len(item.WorkflowObservations) > 0 {
				limit = " Selected workflow observations cover only the latest run/current attempt, not older runs or all jobs; absent failure rows do not certify that jobs were checked."
			}
			if diagnostic != nil {
				copy := *diagnostic
				copy.Explanation += limit
				diagnostic = &copy
			}
		}
		out.Targets = append(out.Targets, CIWatchTargetOutput{item, phase, diagnostic})
	}
	return out
}

func ciWatchDiagnostic(item CIWatchTargetResult) (string, *ReviewOperationDiagnostic) {
	if len(item.WorkflowFailures) > 0 {
		historical := 0
		for _, row := range item.WorkflowFailures {
			if row.Basis == "historical_same_head_failure_supersession_unproven" {
				historical++
			}
		}
		if historical > 0 {
			phase := "workflow_failure"
			if item.WaitingForChange {
				phase = "waiting_for_change"
			}
			explanation := "Confirmed job failure on this pinned PR head. Labelled historical rows identify failed jobs in earlier runs' current numbered attempts. Newer runs have not proven whether they reran or replaced those failed jobs; these rows do not prove that the latest/current comparison still fails. Their run base is unknown. Required CI keeps its separate pending or unproven result, without coverage credit."
			if historical < len(item.WorkflowFailures) {
				explanation += " Unlabelled rows retain the ordinary proven failure-attention meaning."
			}
			action := "Inspect the supplied historical workflow/run/attempt/job IDs for the actual failure; verify or fix it without treating metadata-only success as repair. Reuse observation_key as after_observation_key to wait quietly for changed evidence. Do not rerun CI or change required gates solely to clear this historical signal."
			if item.Reason == "ci_merge_base_mismatch" {
				action += " First allow GitHub to regenerate the current test merge after a retarget or base update; collect a later source refresh. If the mismatch persists with a stable live base, reconcile the actual PR/base comparison through the repository's normal authorized workflow before expecting current required CI."
			}
			return phase, &ReviewOperationDiagnostic{explanation, action}
		}
		if item.WorkflowAttentionBasis == "head_only_current_comparison_unproven" {
			phase := "workflow_failure"
			if item.WaitingForChange {
				phase = "waiting_for_change"
			}
			return phase, &ReviewOperationDiagnostic{"A failed job was observed on this same pinned PR head. The workflow run's base/merge comparison is unknown; current required CI remains unproven because its test-merge base does not match the live branch. This is repair attention, not a current-comparison failure or CI credit.", "Inspect the supplied workflow/run/attempt/job IDs for the failure. Keep the source pins and required-CI refusal; do not treat a metadata-only green run as proof of repair. Reuse observation_key as after_observation_key to await changed attention quietly. First allow GitHub to regenerate the current test merge after a retarget or base update; collect a later source refresh. If the mismatch persists with a stable live base, reconcile the actual PR/base comparison through the repository's normal authorized workflow before expecting current required CI."}
		}
		if item.WaitingForChange {
			return "waiting_for_change", &ReviewOperationDiagnostic{"An explicitly selected workflow still has the failed job identified by after_observation_key. The required gate remains pending; no changed facts were observed before this response.", "Retain the original source comparison and workflow selection when rearming. No CI rerun or required-check change was requested."}
		}
		return "workflow_failure", &ReviewOperationDiagnostic{"A job in an explicitly selected workflow failed while required CI remains pending. This is early repair attention, not proof that the required aggregate gate failed.", "Inspect the supplied workflow, run, attempt and job IDs. To await changed facts after handling this signal, reuse observation_key as after_observation_key with the same pins and workflow selection."}
	}
	if item.WaitingForChange && item.State == "passed" {
		return "waiting_for_change", &ReviewOperationDiagnostic{"Required CI passed as of observed_at with the proof identified by after_observation_key. No changed terminal evidence was observed before this response; the passing checks remain observed facts, not a guarantee that CI will stay passed.", "Retain the original source comparison when rearming. Pending checks alone do not wake this terminal wait. Changed passing proof, failure or selected workflow attention returns normally; this never reruns CI."}
	}
	if item.WaitingForChange {
		return "waiting_for_change", &ReviewOperationDiagnostic{"Required CI still has the failure identified by after_observation_key. No changed result was observed before this response; the failed checks remain observed facts.", "Retain the original source comparison when rearming. A changed failure or a pass will return normally; this never reruns CI."}
	}
	if item.State != "pending" {
		return "", nil
	}
	phase, explanation := "observation_unproven", "The controller has not established a current required-CI result. Zero counts do not mean no checks are required or that CI passed; any collected counts remain informational."
	switch {
	case item.Reason == "ci_watch_check_pending":
		phase, explanation = "not_observed", "The controller has not completed a fresh CI observation for this watch. Zero counts and an empty checks list mean unknown, not that no checks are required."
	case item.Reason == "ci_watch_refresh_pending":
		phase, explanation = "refreshing", "A newer controller check has been requested but has not completed. Any earlier counts and checks are informational until that refresh completes."
	case oneReviewOf(item.Reason, "ci_merge_base_mismatch", "ci_merge_identity_unproven", "ci_merge_pending",
		"ci_provider_inventory_incomplete", "ci_provider_inventory_incomplete_head_results", "ci_provider_inventory_incomplete_merge_results", "ci_provider_inventory_incomplete_results_recheck", "ci_provider_inventory_incomplete_merge_results_recheck",
		"ci_provider_invalid_response_head_results", "ci_provider_invalid_response_merge_results", "ci_provider_invalid_response_results_recheck", "ci_provider_invalid_response_merge_results_recheck", "ci_provider_invalid_response_assessment",
		"ci_provider_invalid_response_check_run_time_order", "ci_provider_invalid_response_head_results_check_run_time_order", "ci_provider_invalid_response_merge_results_check_run_time_order", "ci_provider_invalid_response_results_recheck_check_run_time_order", "ci_provider_invalid_response_merge_results_recheck_check_run_time_order"):
		if diagnostic := AgentReviewCI(ReviewSourceCIAssessment{State: "unproven", Reason: item.Reason, Target: ReviewSourceObservation{HeadSHA: item.ObservedHeadSHA, BaseSHA: item.ObservedBaseSHA}, MergeBaseMismatch: item.MergeBaseMismatch}).Diagnostic; diagnostic != nil {
			return "observation_unproven", &ReviewOperationDiagnostic{diagnostic.Explanation, diagnostic.NextAction}
		}
	case item.Reason == "ci_observation_changed":
		// Retained counts are informational until a coherent observation exists.
	case item.ObservedAt != "" && item.Unproven > 0:
		phase, explanation = "checks_unproven", "The latest observation is missing required-check evidence or could not verify it as of observed_at. Inspect the listed check contexts and reasons; this is not a CI pass."
	case item.ObservedAt != "" && item.Pending > 0:
		if diagnostic := pendingCommitStatusDiagnostic(item.Checks); diagnostic != nil {
			return "checks_pending", &ReviewOperationDiagnostic{diagnostic.Explanation, diagnostic.NextAction + " This bounded wait has ended. Start another bounded CI wait for the remaining targets when you need a newer result."}
		}
		phase, explanation = "checks_pending", "The latest observation reports required GitHub checks still pending as of observed_at. Inspect the listed check contexts and statuses."
	}
	return phase, &ReviewOperationDiagnostic{explanation, "Start another bounded CI wait for the remaining targets when you need a newer result."}
}
