package client

import (
	"errors"
	"net/http"
)

// Keep reattachment identity from the validated original read, never from
// provider state or arbitrary server text. This does not retry the read.
func ciWatchTargetError(err error, operationID string, targets []CIWatchTarget) error {
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
	ciRefusal := operationID == "" && ValidateCIWatchTargets(targets) == nil && ciWatchRefusalReason(details["reason"]) &&
		(failure.Code == "overloaded" && failure.Status == http.StatusServiceUnavailable || failure.Code == "storage_unavailable" && failure.Status == http.StatusInternalServerError)
	transport := failure.Status == 0 && failure.Code == "connection_failed"
	gateway := failure.Code == "service_unavailable" && (failure.Status == http.StatusBadGateway || failure.Status == http.StatusServiceUnavailable || failure.Status == http.StatusGatewayTimeout)
	if !shutdown && !local && !ciRefusal && !transport && !gateway {
		return err
	}
	if operationID != "" {
		if ValidateReviewRunSelectionID(operationID) == nil {
			details["selection_id"] = operationID
		} else {
			details["operation_id"] = operationID
		}
	} else {
		pinned := append([]CIWatchTarget(nil), targets...)
		for i := range pinned {
			pinned[i].FailureWorkflowIDs = append([]int64(nil), pinned[i].FailureWorkflowIDs...)
		}
		details["targets"] = pinned
	}
	result := *failure
	result.Details = details
	return &result
}

func ciWatchRefusalReason(value any) bool {
	switch value {
	case "ci_watch_scope_read", "ci_watch_binding_read", "ci_watch_capacity", "ci_watch_observer_unavailable", "ci_watch_canceled":
		return true
	}
	return false
}
