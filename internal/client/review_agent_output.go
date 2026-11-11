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
