package reviewexec

import (
	"context"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type runSourceBudgetExceeded struct{}

func (runSourceBudgetExceeded) Error() string { return "selection source budget exhausted" }
func (runSourceBudgetExceeded) Unwrap() error { return context.DeadlineExceeded }

var errRunSourceBudget error = runSourceBudgetExceeded{}

// Only finite classifications cross the retained selection boundary. Provider
// messages, URLs, response bodies and arbitrary adapter details never do.
func runReason(err error) string {
	if errors.Is(err, errRunSourceBudget) {
		return "source_budget_exhausted"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "source_fetch_timeout"
	}
	var provider *reviewprovider.Error
	if errors.As(err, &provider) {
		switch provider.Code {
		case "provider_rate_limited":
			return "source_rate_limited"
		case "provider_unauthorized":
			return "source_unauthorized"
		case "provider_not_found":
			return "source_not_found"
		case "provider_invalid_response":
			return "source_response_invalid"
		case "provider_unavailable":
			return "source_provider_unavailable"
		}
	}
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "review_held" {
		if details, ok := failure.Details.(map[string]string); ok {
			switch reason := details["reason"]; reason {
			case "repository_changed", "target_changed", "parent_identity_changed", "parent_unavailable", "default_base_changed", "parent_moved", "observation_incomplete", "comparison_changed":
				return reason
			}
		}
	}
	return "source_unavailable"
}
