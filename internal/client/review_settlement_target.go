package client

import "math"

// ReviewSettlementTarget supplies original-attempt guards. It does not assert
// settlement eligibility or that external work has stopped.
type ReviewSettlementTarget struct {
	AttemptRevision int64  `json:"attempt_revision"`
	RepositoryID    int64  `json:"repository_id"`
	HeadSHA         string `json:"head_sha"`
	ExternalID      string `json:"external_id"`
}

func validReviewSettlementTarget(v *ReviewSettlementTarget, attemptID string) bool {
	return v == nil || validReviewOpaqueID(attemptID, "review_attempt_") && v.AttemptRevision > 0 && v.AttemptRevision < math.MaxInt64 && v.RepositoryID > 0 && len(v.HeadSHA) == 40 && validReviewSHA(v.HeadSHA) && (v.ExternalID == "" || validExecutionIdentifier(v.ExternalID))
}
