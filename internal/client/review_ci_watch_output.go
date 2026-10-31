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
	Targets []CIWatchTargetOutput `json:"targets"`
}

func AgentCIWatch(value CIWatchResult) CIWatchOutput {
	out := CIWatchOutput{CIWatchResult: value, Targets: make([]CIWatchTargetOutput, 0, len(value.Targets))}
	for _, item := range value.Targets {
		phase, diagnostic := ciWatchDiagnostic(item)
		out.Targets = append(out.Targets, CIWatchTargetOutput{item, phase, diagnostic})
	}
	return out
}

func ciWatchDiagnostic(item CIWatchTargetResult) (string, *ReviewOperationDiagnostic) {
	if item.State != "pending" {
		return "", nil
	}
	phase, explanation := "observation_unproven", "The controller has not established a current required-CI result. Zero counts do not mean no checks are required or that CI passed; any collected counts remain informational."
	switch {
	case item.Reason == "ci_watch_check_pending":
		phase, explanation = "not_observed", "The controller has not completed a fresh CI observation for this watch. Zero counts and an empty checks list mean unknown, not that no checks are required."
	case item.Reason == "ci_watch_refresh_pending":
		phase, explanation = "refreshing", "A newer controller check has been requested but has not completed. Any earlier counts and checks are informational until that refresh completes."
	case oneReviewOf(item.Reason, "ci_merge_base_mismatch", "ci_merge_identity_unproven", "ci_merge_pending"):
		if diagnostic := AgentReviewCI(ReviewSourceCIAssessment{State: "unproven", Reason: item.Reason}).Diagnostic; diagnostic != nil {
			return "observation_unproven", &ReviewOperationDiagnostic{diagnostic.Explanation, diagnostic.NextAction}
		}
	case item.ObservedAt != "" && item.Pending > 0:
		phase, explanation = "checks_pending", "The latest observation reports required GitHub checks still pending as of observed_at. Inspect the listed check contexts and statuses."
	case item.ObservedAt != "" && item.Unproven > 0:
		phase, explanation = "checks_unproven", "The latest observation is missing required-check evidence or could not verify it as of observed_at. Inspect the listed check contexts and reasons; this is not a CI pass."
	}
	return phase, &ReviewOperationDiagnostic{explanation, "Start another bounded CI wait for the remaining targets when you need a newer result."}
}
