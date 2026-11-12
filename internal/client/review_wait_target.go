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
	workflowRefusal := failure.Code == "ci_watch_workflow_capacity" && failure.Status == http.StatusServiceUnavailable ||
		failure.Code == "ci_watch_workflow_failures_unsupported" && failure.Status == http.StatusConflict
	ciRefusal := operationID == "" && ValidateCIWatchTargets(targets) == nil && (workflowRefusal ||
		ciWatchRefusalReason(details["reason"]) && (failure.Code == "overloaded" && failure.Status == http.StatusServiceUnavailable ||
			failure.Code == "storage_unavailable" && failure.Status == http.StatusInternalServerError))
	if !shutdown && !local && !ciRefusal {
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
	if operationID == "" && (local || ciRefusal) {
		for _, target := range targets {
			if target.BaseSHA == "" {
				result.Action += " At least one base was implicit. Verify the original comparison and supply explicit base pins before treating another bounded CI wait as the same comparison."
				break
			}
		}
	}
	return &result
}
