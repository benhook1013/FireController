package client

// These are runtime health facts, independent of immutable snapshot age.
type ReviewObserverHealth struct {
	State     string `json:"state"`
	StartedAt string `json:"started_at,omitempty"`
}
type ReviewPullObservationHealth struct {
	State           string `json:"state"`
	CheckedAt       string `json:"checked_at,omitempty"`
	LastSuccessAt   string `json:"last_success_at,omitempty"`
	NextCheckAt     string `json:"next_check_at,omitempty"`
	Error           string `json:"error,omitempty"`
	CICheckedAt     string `json:"ci_checked_at,omitempty"`
	CIError         string `json:"ci_error,omitempty"`
	HostedCheckedAt string `json:"hosted_checked_at,omitempty"`
	HostedError     string `json:"hosted_error,omitempty"`
}

func validObserverHealth(h *ReviewObserverHealth) bool {
	if h == nil {
		return true
	}
	return oneReviewOf(h.State, "running", "unavailable", "unconfigured") && (h.State == "running" && controlTime(h.StartedAt) || h.State != "running" && h.StartedAt == "")
}
func validPullObservationHealth(h *ReviewPullObservationHealth) bool {
	if h == nil {
		return true
	}
	if !oneReviewOf(h.State, "not_enrolled", "unavailable", "waiting", "current", "comparison_changed", "overdue", "retrying") {
		return false
	}
	for _, s := range []string{h.CheckedAt, h.LastSuccessAt, h.NextCheckAt, h.CICheckedAt, h.HostedCheckedAt} {
		if s != "" && !controlTime(s) {
			return false
		}
	}
	for _, s := range []string{h.Error, h.CIError, h.HostedError} {
		if s != "" && !validExecutionIdentifier(s) {
			return false
		}
	}
	return (h.State != "current" && h.State != "comparison_changed" || h.LastSuccessAt != "") && (h.State != "retrying" || h.Error != "")
}
