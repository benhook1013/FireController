package client

type CIWorkflowObservation struct {
	WorkflowID int64  `json:"workflow_id"`
	Basis      string `json:"basis"`
	RunID      int64  `json:"run_id,omitempty"`
	RunAttempt int64  `json:"run_attempt,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

func validCIWatchWorkflowObservations(item CIWatchTargetResult, pin CIWatchTarget) bool {
	if item.WorkflowObservations == nil {
		return true
	}
	if len(item.WorkflowObservations) == 0 || len(item.WorkflowObservations) > 16 || len(item.WorkflowObservations) != len(pin.FailureWorkflowIDs) || item.ObservationKey == "" || item.State != "pending" || item.Reason == "ci_watch_refresh_pending" || item.ObservedHeadSHA != pin.HeadSHA || item.ObservedBaseSHA == "" || pin.BaseSHA != "" && item.ObservedBaseSHA != pin.BaseSHA || item.ObservedAt == "" || item.BaseRef == "" || item.GateRef == "" || item.Selection == "" || item.Selection == "head" && item.CICommitSHA != pin.HeadSHA {
		return false
	}
	selected, seen := map[int64]bool{}, map[int64]bool{}
	for _, id := range pin.FailureWorkflowIDs {
		selected[id] = true
	}
	for _, row := range item.WorkflowObservations {
		if !selected[row.WorkflowID] || seen[row.WorkflowID] || row.Basis != "latest_run_only" {
			return false
		}
		seen[row.WorkflowID] = true
		if row.RunID == 0 {
			if row.RunAttempt != 0 || row.Status != "" || row.Conclusion != "" {
				return false
			}
		} else if row.RunID < 0 || row.RunAttempt <= 0 || len(row.Status) > 64 || !validExecutionIdentifier(row.Status) || row.Conclusion != "" && (len(row.Conclusion) > 64 || !validExecutionIdentifier(row.Conclusion)) {
			return false
		}
		for _, failure := range item.WorkflowFailures {
			if failure.WorkflowID == row.WorkflowID && (failure.RunID != row.RunID || failure.RunAttempt != row.RunAttempt) {
				return false
			}
		}
	}
	return true
}
