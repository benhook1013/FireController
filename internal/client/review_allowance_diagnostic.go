package client

func hostedAllowanceFailureExplanation(reason string) string {
	switch reason {
	case "hosted_allowance_history_invalid":
		return "Hosted preparation received provider query history that could not be validated."
	case "hosted_allowance_history_unavailable":
		return "Hosted preparation could not complete the provider query-history read."
	case "hosted_allowance_capture_invalid":
		return "Hosted preparation could not validate the provider source for its original allowance-query capture."
	case "hosted_allowance_capture_unavailable":
		return "Hosted preparation could not safely retain or read its original allowance-query capture."
	case "hosted_allowance_query_post_unproven":
		return "Delivery of the non-review allowance query could not be established."
	case "hosted_allowance_query_recording_pending":
		return "The non-review allowance query response could not be confirmed in the local journal."
	case "hosted_allowance_capture_recording_pending":
		return "The original allowance-query capture reference could not be recorded in the local journal."
	default:
		return ""
	}
}

func hostedAllowanceFailureDiagnostic(channel, state, reason string) *ReviewOperationDiagnostic {
	if channel != "hosted" || state != "held" {
		return nil
	}
	explanation := hostedAllowanceFailureExplanation(reason)
	if explanation == "" {
		return nil
	}
	return &ReviewOperationDiagnostic{
		Explanation: explanation + " No review trigger was posted by this attempt at this preparation guard. This establishes neither exhausted allowance nor a particular timestamp defect.",
		NextAction:  "Continue other work; no human confirmation, manual query, clock adjustment or grant reset is needed. This hold is terminal. If the planned review remains authorized, one later fresh request/key rechecks current source and allowance; the original key replays this hold. No retry deadline was established.",
	}
}
