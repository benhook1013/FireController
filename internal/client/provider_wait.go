package client

import "time"

// ValidateReviewOperationWait admits a bounded wait without changing the
// identity or lifetime of the requested review operation.
func ValidateReviewOperationWait(seconds int) error {
	if seconds < 0 || seconds > 900 {
		return validation()
	}
	return nil
}

// ReviewOperationBudget gives validated review waits transport headroom. The
// caller's own earlier deadline remains authoritative.
func ReviewOperationBudget(seconds int) time.Duration { return providerWaitBudget(seconds) }

func providerWaitBudget(seconds int) time.Duration {
	if seconds > 2 {
		return time.Duration(seconds+5) * time.Second
	}
	return requestTimeout
}
