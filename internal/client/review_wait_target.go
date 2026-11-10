package client

import (
	"errors"
	"net/http"
)

// Preserve only the validated caller's original operation identity on interruption.
func reviewWaitTargetError(err error, operationID string) error {
	var failure *Error
	if !errors.As(err, &failure) {
		return err
	}
	details := map[string]any{}
	if original, ok := failure.Details.(map[string]any); ok {
		for key, value := range original {
			details[key] = value
		}
	} else if failure.Details != nil {
		return err
	}
	shutdown := failure.Status == http.StatusServiceUnavailable && failure.Code == "overloaded" && details["reason"] == "result_wait_lifetime"
	local := failure.Status == 0 && oneReviewOf(failure.Code, "canceled", "deadline_exceeded")
	if !shutdown && !local {
		return err
	}
	details["operation_id"] = operationID
	result := *failure
	result.Details = details
	return &result
}
