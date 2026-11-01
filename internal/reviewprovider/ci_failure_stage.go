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
