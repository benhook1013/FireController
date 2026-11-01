package client

import (
	"errors"
	"net/http"
)

// Keep reattachment identity from the validated original read, never from
// provider state or arbitrary server text. This does not retry the read.
func reviewWaitTargetError(err error, operationID string, targets []CIWatchTarget) error {
	var failure *Error
	if !errors.As(err, &failure) {
		return err
	}
	details := map[string]any{}
	if original, ok := failure.Details.(map[string]any); ok {
		for key, value := range original {
			details[key] = value
		}
	} else if original, ok := failure.Details.(map[string]string); ok {
		for key, value := range original {
			details[key] = value
		}
	} else if failure.Details != nil {
		return err // Preserve any future, differently typed diagnostic intact.
	}
	shutdown := failure.Status == http.StatusServiceUnavailable && failure.Code == "overloaded" && details["reason"] == "result_wait_lifetime"
	local := failure.Status == 0 && oneReviewOf(failure.Code, "canceled", "deadline_exceeded")
	if !shutdown && !local {
		return err
	}
	if operationID != "" {
		details["operation_id"] = operationID
	} else {
		details["targets"] = append([]CIWatchTarget(nil), targets...)
	}
	result := *failure
	result.Details = details
	return &result
}
