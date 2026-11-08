package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// preparationFailure preserves error identity for existing guards, but its
// printable diagnostic contains only maintained stage and cause codes.
type preparationFailure struct {
	stage string
	cause string
	err   error
}

func (e *preparationFailure) Error() string    { return "preparation_" + e.stage + "_" + e.cause }
func (e *preparationFailure) String() string   { return e.Error() }
func (e *preparationFailure) GoString() string { return e.Error() }
func (e *preparationFailure) Unwrap() error    { return e.err }

// PreparationFailureAt attributes an otherwise generic preparation failure.
// Known held reasons still take precedence at the supervisor boundary. Neither
// arbitrary stage strings nor error messages/details become retained reasons.
func PreparationFailureAt(stage string, err error) error {
	if err == nil {
		return nil
	}
	switch stage {
	case "execution_context", "source_resolution", "workspace", "retry_baseline", "source_refresh", "allowance_recording", "attempt_publication", "allowance_wait", "trigger_intent":
	default:
		stage = "unknown"
	}
	return &preparationFailure{stage: stage, cause: preparationFailureCause(err), err: err}
}

func preparationFailureCause(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "not_found"
	}
	var local *core.Error
	if errors.As(err, &local) {
		switch local.Code {
		case "overloaded", "storage_unavailable", "review_conflict", "validation_failed", "not_found", "review_projection_missing", "observation_conflict":
			return local.Code
		}
	}
	var provider *reviewprovider.Error
	if errors.As(err, &provider) {
		switch provider.Code {
		case "provider_rate_limited", "provider_unauthorized", "provider_not_found", "provider_invalid_response", "provider_unavailable":
			return provider.Code
		}
	}
	return "other"
}
