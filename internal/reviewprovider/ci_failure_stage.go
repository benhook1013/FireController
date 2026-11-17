package reviewprovider

import "errors"

// CI failures retain one static collection stage, never an endpoint URL,
// credential, response body or provider message. The original error identity
// remains available to the service's existing safe classification.
type ciCollectionError struct {
	stage string
	cause error
}

func (e *ciCollectionError) Error() string             { return "Required CI collection failed." }
func (e *ciCollectionError) Unwrap() error             { return e.cause }
func (e *ciCollectionError) CICollectionStage() string { return e.stage }

// CICollectionStage returns only supported static diagnostics. Reader mocks
// and wrappers may participate without their arbitrary text reaching storage.
func CICollectionStage(err error) string {
	var failure interface{ CICollectionStage() string }
	if !errors.As(err, &failure) {
		return ""
	}
	stage := failure.CICollectionStage()
	switch stage {
	case "authority", "candidate", "merge_results", "head_results", "actions", "workflow_identity", "attempt", "assessment", "authority_recheck", "candidate_recheck", "results_recheck", "actions_recheck", "merge_results_recheck":
		return stage
	}
	return ""
}

type ciResultIssueError struct {
	cause error
	issue string
}

func (e *ciResultIssueError) Error() string         { return "CI result validation failed." }
func (e *ciResultIssueError) Unwrap() error         { return e.cause }
func (e *ciResultIssueError) CIResultIssue() string { return e.issue }

// CIResultIssue exposes a finite validation fact, never provider values or text.
// The wrapped provider error keeps its original code and retry classification.
func CIResultIssue(err error) string {
	var failure interface{ CIResultIssue() string }
	if errors.As(err, &failure) && failure.CIResultIssue() == "check_run_time_order" {
		return "check_run_time_order"
	}
	return ""
}
