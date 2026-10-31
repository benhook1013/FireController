package reviews

// MergeStatus is an as-of browser projection of GitHub restrictions and the
// retained required-CI assessment. It is not a merge action or review verdict.
type MergeStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func mergeStatus(p PullSummary) *MergeStatus {
	if p.State == "merged" || p.State == "closed" {
		return nil
	}
	result := func(state, reason string) *MergeStatus { return &MergeStatus{state, reason} }
	if p.State != "open" {
		return result("unknown", "pull_state_unknown")
	}
	if p.IsDraft != nil && *p.IsDraft {
		return result("blocked", "draft")
	}
	if p.Mergeability == "conflicting" || p.mergeStateStatus == "DIRTY" {
		return result("blocked", "merge_conflicts")
	}
	if p.CIState == "failure" {
		return result("blocked", "required_ci_failed")
	}
	switch p.mergeStateStatus {
	case "BLOCKED", "BEHIND", "DRAFT":
		return result("blocked", "github_"+map[string]string{"BLOCKED": "blocked", "BEHIND": "behind", "DRAFT": "draft"}[p.mergeStateStatus])
	}
	if p.CIState == "pending" {
		return result("pending", "required_ci_pending")
	}
	if p.CIState != "success" && p.CIState != "not_required" {
		return result("unknown", "required_ci_unknown")
	}
	if p.Mergeability != "mergeable" {
		return result("unknown", "merge_state_unknown")
	}
	switch p.mergeStateStatus {
	case "CLEAN", "HAS_HOOKS", "UNSTABLE":
		// UNSTABLE includes optional failing checks. A satisfied required-CI
		// assessment distinguishes those from checks that actually block merge.
		return result("ready", "github_and_required_ci_clear")
	}
	return result("unknown", "merge_state_unknown")
}

func includeMergeStatus(items []PullSummary) {
	for i := range items {
		items[i].MergeStatus = mergeStatus(items[i])
	}
}
