package client

func sourceHoldAction(state, reason string) string {
	if state != "held" {
		return ""
	}
	switch reason {
	case "default_base_changed":
		return "This comparison has no selected unmerged parent, but the PR targets a branch other than the repository default. Check the PR base and current default branch; reconcile/retarget the PR as authorized, then refresh its current head with a new key. This is a PR comparison hold, not an identity or dashboard-refresh failure."
	case "parent_moved":
		return "The published PR comparison no longer matches the selected parent head. Reconcile and publish the branch repair under existing project authorization and active-review safety, then refresh the new published head with a new key. Source refresh checks review comparisons; it is not permission to push and cannot approve an unpublished local candidate."
	case "target_changed":
		return "The PR state or head differs from the requested comparison. Read the current queue/source facts and refresh the intended current open head with a new key."
	case "parent_unavailable", "parent_identity_changed":
		return "The configured parent is unavailable or no longer matches. Check the intended stack and repository configuration before refreshing the comparison."
	case "observation_incomplete", "provider_unavailable":
		return "The provider comparison could not be completed. Check automatic-observation health and retry the intended refresh later with a new key; the original held operation stays retained."
	default:
		return "Read the retained hold reason and current project review configuration before retrying. Preserve the original operation/key for recovery; a held result does not establish readiness."
	}
}
