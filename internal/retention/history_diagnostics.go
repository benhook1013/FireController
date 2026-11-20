package retention

import (
	"context"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
)

type historyPhaseError struct {
	phase string
	cause error
}

func (e *historyPhaseError) Error() string { return "review history " + e.phase + " failed" }
func (e *historyPhaseError) Unwrap() error { return e.cause }

func historyFailure(phase string, cause error) error {
	if cause == nil {
		return nil
	}
	return &historyPhaseError{phase: phase, cause: cause}
}

func historyErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var failure *core.Error
	if errors.As(err, &failure) {
		switch failure.Code {
		case "overloaded":
			// The common writer intentionally normalizes busy and deadlines.
			return "deadline_or_busy"
		case "storage_unavailable":
			return "storage"
		}
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 255 {
		case 5, 6:
			return "busy"
		case 19:
			return "constraint"
		case 9:
			return "interrupted"
		}
		return "storage"
	}
	return "other"
}
