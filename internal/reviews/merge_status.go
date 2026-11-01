package reviews

// MergeStatus projects GitHub's observed merge state for the browser. Required
// CI is separate; this is not a merge action or controller review verdict.
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
	switch p.mergeStateStatus {
	case "CLEAN":
		return result("ready", "github_clean")
	case "BLOCKED":
		return result("blocked", "github_blocked")
	case "BEHIND":
		return result("blocked", "github_behind")
	case "DIRTY":
		return result("blocked", "github_dirty")
	case "DRAFT":
		return result("blocked", "github_draft")
	case "UNSTABLE":
		return result("pending", "github_unstable")
	case "HAS_HOOKS":
		return result("pending", "github_has_hooks")
	}
	return result("unknown", "merge_state_unknown")
}

func includeMergeStatus(items []PullSummary) {
	for i := range items {
		items[i].MergeStatus = mergeStatus(items[i])
	}
}
