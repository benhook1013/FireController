package client

// ReviewHostedObserverStatus describes observation of the original issued
// Hosted attempt. It neither grants completion nor authorizes a replacement.
// Unknown means no observer facts are available; absence supports older servers.
type ReviewHostedObserverStatus struct {
	State        string `json:"state"`
	LastCheckAt  string `json:"last_check_at,omitempty"`
	LastSourceAt string `json:"last_source_at,omitempty"`
	NextCheckAt  string `json:"next_check_at,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

func validReviewHostedObserver(v *ReviewHostedObserverStatus, channel, state, attempt string) bool {
	if v == nil {
		return true
	}
	if channel != "hosted" || !oneReviewOf(state, "posting", "active", "uncertain") || !validReviewOpaqueID(attempt, "review_attempt_") || !oneReviewOf(v.State, "unknown", "checking", "waiting", "retrying", "stopped") {
		return false
	}
	if v.State == "unknown" {
		return v.LastCheckAt == "" && v.LastSourceAt == "" && v.NextCheckAt == "" && v.Reason == "" && v.Error == ""
	}
	for _, instant := range []string{v.LastCheckAt, v.LastSourceAt, v.NextCheckAt} {
		if instant != "" && !validAllowanceTime(instant) {
			return false
		}
	}
	if v.LastSourceAt != "" && v.LastCheckAt == "" || v.Reason != "" && !validExecutionIdentifier(v.Reason) || v.Error != "" && !validExecutionIdentifier(v.Error) {
		return false
	}
	return (v.State != "retrying" || v.Error != "") && (v.State != "waiting" || v.Error == "")
}
