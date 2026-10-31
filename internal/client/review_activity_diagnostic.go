package client

// Activity guards run before the review POST. These are retained failure
// classes, not current provider observations or permission to retry.
func hostedActivityDiagnostic(channel, state, reason string) *ReviewOperationDiagnostic {
	if channel != "hosted" || state != "held" && state != "failed" {
		return nil
	}
	var explanation string
	action := "This operation is terminal. Continue other work. If the planned review remains authorized, one later fresh request/key with the current configuration/head rechecks provider activity and normal allowance/admission. Do not manually trigger a review, reset grants, assume quota exhaustion or bypass the activity guard. Keep the original key for an unknown action outcome."
	switch reason {
	case "provider_activity_rate_limited":
		explanation = "GitHub rate-limited the provider activity read. This is GitHub API capacity, not CodeRabbit review allowance or this PR's round limit."
	case "provider_activity_unauthorized":
		explanation = "GitHub rejected the configured provider credential during the activity read."
		action = "Verify the configured GitHub provider access through the existing project/operator setup; this does not establish a problem with the worker's controller identity. Once access is corrected, a still-authorized new request with current configuration/head rechecks all guards. Do not reset grants or directly trigger a review."
	case "provider_activity_not_found":
		explanation = "GitHub could not find the repository/head resource needed for the provider activity read."
		action = "Verify that this request's repository and head are still available. Use the current qualified comparison for any still-authorized new request. Do not reset grants or bypass the missing-source guard."
	case "provider_activity_response_invalid":
		explanation = "The provider activity response could not be validated; it did not establish whether a review was active."
	case "provider_activity_unavailable":
		explanation = "The provider activity read could not complete. This does not prove a provider outage, running review or exhausted allowance."
	case "provider_activity_ambiguous":
		explanation = "The provider activity observations conflicted, so the guard could not establish an idle provider."
		action = "Inspect the provider checks/statuses for this operation's head and let the conflicting activity resolve. Then a still-authorized new request rechecks the provider and all normal guards. Do not bypass the guard or reset grants."
	case "provider_activity_state_unknown":
		explanation = "The selected provider progress signal had an unrecognized or inconclusive state."
		action = "Inspect the provider checks/statuses for this operation's head. Once its state is understood, a still-authorized new request rechecks provider activity and normal admission. Do not bypass the guard or reset grants."
	case "provider_activity_observation_invalid":
		explanation = "The provider activity observation had missing, stale or future timing and could not establish an idle provider."
	case "provider_review_active":
		explanation = "The selected provider progress signal reported activity at this request's guard."
		action = "Wait for that provider activity on this operation's head to finish. A later still-authorized new request rechecks the live signal and normal allowance/admission. This terminal operation does not resume itself; do not submit overlapping review triggers or reset grants."
	case "provider_activity_unproven", "provider_activity_changed":
		explanation = "The provider activity guard could not establish an idle provider. The exact API or progress-signal subcause was not retained for this operation; a current observation cannot reconstruct it."
	default:
		return nil
	}
	return &ReviewOperationDiagnostic{Explanation: explanation + " No review trigger was posted by this attempt at this guard; this result is separate from allowance queries and completed review evidence.", NextAction: action}
}
