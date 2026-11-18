package client

type ReviewOperationDiagnostic struct {
	Explanation string `json:"explanation"`
	NextAction  string `json:"next_action"`
}
type ReviewSelectionOutput struct {
	ReviewRunState
	Diagnostic *ReviewOperationDiagnostic `json:"diagnostic,omitempty"`
}

func AgentReviewSelection(value ReviewRunState) ReviewSelectionOutput {
	out := ReviewSelectionOutput{ReviewRunState: value}
	if value.Selection.State == "held" && value.Selection.OperationID == "" && value.Operation == nil {
		out.Diagnostic = selectionSourceDiagnostic(value.Selection.Reason)
	}
	return out
}
func draftReadinessDiagnostic() *ReviewOperationDiagnostic {
	return &ReviewOperationDiagnostic{"A source check observed the PR as a GitHub Draft. Draft marks initial review readiness; it does not erase earned review progress.", "After marking the PR ready for review, if this review remains authorized, use one fresh request with a new key and current configuration/head. The old key replays this terminal hold. Do not reset rounds, grant extra reviews or retry automatically; normal admission and provider checks still apply."}
}

func selectionSourceDiagnostic(reason string) *ReviewOperationDiagnostic {
	if reason == "pull_draft" {
		return draftReadinessDiagnostic()
	}
	var explanation string
	switch reason {
	case "source_budget_exhausted":
		explanation = "Selection exhausted its source-acquisition budget before establishing the target; this is not provider quota exhaustion."
	case "source_fetch_timeout":
		explanation = "A required source fetch timed out before selection established the target."
	case "source_rate_limited":
		explanation = "A required source read was rate limited; this selection retains no exact retry deadline."
	case "source_unauthorized":
		explanation = "The provider refused authorization for a required selection source read."
	case "source_not_found":
		explanation = "A required source lookup was not found; this does not identify the intended target as the missing PR."
	case "source_response_invalid":
		explanation = "A required source response failed validation."
	case "source_provider_unavailable":
		explanation = "A required source provider read could not be completed."
	case "source_unavailable":
		explanation = "The required source facts could not be established. This historical selection did not retain its exact failure cause; do not infer a timeout, quota or authorization failure."
	default:
		return nil
	}
	return &ReviewOperationDiagnostic{explanation, "This held selection is terminal and has no child operation. After addressing the source issue, if the review remains authorized, use one fresh reviews start / review_start with a new key and current configuration. The old key replays this hold. Keep pending selections and selected child operations on their original identities. No round reset or extra grant is needed; normal front, source and provider checks still apply."}
}

func AgentReviewRunResult(value ReviewRunResult) any {
	if value.Result != nil && value.Receipt == nil {
		return AgentReviewSelection(*value.Result)
	}
	return value
}

// Retained wait guidance preserves the original request and never starts work.
type ReviewOperationOutput struct {
	ReviewOperation
	Diagnostic *ReviewOperationDiagnostic `json:"diagnostic,omitempty"`
}
type ReviewOperationStatusOutput struct {
	ReviewOperationStatus
	Diagnostic *ReviewOperationDiagnostic `json:"diagnostic,omitempty"`
}

func retainedWaitDiagnostic(wait *ReviewOperationWait, channel, state, reason, queuedCause string) *ReviewOperationDiagnostic {
	if wait != nil {
		if diagnostic := operationWaitDiagnostic(wait, channel); diagnostic != nil {
			return diagnostic
		}
	}
	if state == "queued" && channel == "cli" && queuedCause == "cli_quota_wait" {
		return &ReviewOperationDiagnostic{"This request is queued for shared CLI quota rechecks; no review is running.", "Keep this original request queued and continue other work. Do not submit a duplicate or reset rounds."}
	}
	if state == "held" && channel == "cli" && reason == "allowance_exhausted" {
		return &ReviewOperationDiagnostic{"This historical CLI quota hold is terminal; it completed no review.", "If still authorized, retry later with a new request and current configuration/head. The original key replays this hold."}
	}
	return nil
}
func AgentReviewOperation(value ReviewOperation) ReviewOperationOutput {
	return ReviewOperationOutput{value, retainedWaitDiagnostic(value.Wait, value.Channel, value.State, value.Reason, value.queuedCause)}
}
func AgentReviewOperationStatus(value ReviewOperationStatus) ReviewOperationStatusOutput {
	out := ReviewOperationStatusOutput{value, retainedWaitDiagnostic(value.Wait, value.Channel, value.State, value.Reason, value.queuedCause)}
	if value.State == "held" && value.Channel == "cli" && value.Reason == "allowance_exhausted" {
		out.NextAction = "retry_later_with_new_request"
	}
	return out
}
func operationWaitDiagnostic(w *ReviewOperationWait, channel string) *ReviewOperationDiagnostic {
	if channel == "cli" && w.RetryNotBefore != "" {
		if w.Paused {
			return &ReviewOperationDiagnostic{"This CLI request is paused before dispatch. Its comparison and quota-recheck schedule are retained; no review is running.", "Resume or cancel this same undispatched request with current revision/key. Resume preserves the comparison and still rechecks shared account quota and admission. It does not grant allowance or interrupt another review."}
		}
		return &ReviewOperationDiagnostic{"Included CLI quota was exhausted. This same request remains queued and automatically rechecks at or after retry_not_before, using bounded backoff shared across projects on the provider account. This is a server recheck time, not the provider reset or a promised start; no review is running.", "Keep this original request queued and continue other work. Pause/cancel only the undispatched request if needed; do not submit a duplicate or reset rounds. Before starting, the controller rechecks the pinned comparison, access, findings, publication safety and current allowance. A changed comparison is held for requalification rather than silently followed."}
	}
	scope, candidates := "", ""
	if w.RetryNotBefore != "" {
		scope = " This deadline schedules this request only; the provider limit’s scope across other PRs, repositories or accounts is unknown."
		candidates = " Other authorized candidates still need their own admission and fresh provider checks."
	}
	if w.Paused {
		retained := "Its review intent is retained; no retry deadline is recorded and no review is running."
		if w.RetryNotBefore != "" {
			retained = "Its retry deadline and review intent are retained; no review is running."
		}
		return &ReviewOperationDiagnostic{"This same operation is paused before provider dispatch. " + retained + scope, "Resume/cancel only this undispatched operation with current revision/key. Pause/resume cannot edit head or out-of-order intent. For an authorized replacement, follow review_operation_control guidance and respect any known cooldown; its timer is not transferred." + candidates}
	}
	if w.RetryNotBefore != "" {
		return &ReviewOperationDiagnostic{"The provider reported a cooldown. This same operation automatically rechecks admission and allowance at or after retry_not_before; the deadline is not a reserved start time or review completion." + scope, "Keep this request for automatic retry. For an authorized intent change, see queued-only review_operation_control cancellation (CLI reviews operation-control); respect the known cooldown because its timer is not transferred. Pause/resume cannot edit head or out-of-order intent." + candidates}
	}
	return nil
}
