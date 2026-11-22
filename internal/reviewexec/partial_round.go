package reviewexec

import "context"

// CLIUsablePartialReason marks a normally returned CLI result whose reviewed
// paths were verified as a nonempty proper subset of the admitted comparison.
// Historic coverage_incomplete also includes invalid file claims.
const CLIUsablePartialReason = "coverage_partial"

func usablePartialOperation(operation Operation) bool {
	return operation.Channel == "cli" && operation.State == "failed" && operation.Reason == CLIUsablePartialReason
}

func usablePartialCapture(capture ReviewEvidenceInput) bool {
	review := capture.Review
	if capture.Hosted != nil || review.Complete || review.Outcome != "failed" || len(review.ReviewedFiles) == 0 ||
		review.ReportedFindings != int64(len(review.Findings)) {
		return false
	}
	seen := make(map[string]bool, len(review.ReviewedFiles))
	for _, path := range review.ReviewedFiles {
		if seen[path] {
			return false
		}
		seen[path] = true
	}
	return true
}

// finalizableEvidence uses the terminal attempt as the execution receipt.
// Partial results also prove the durable operation and admitted comparison.
func finalizableEvidence(ctx context.Context, q EvidenceQuery, evidence ReviewEvidence, attempt Attempt) (bool, error) {
	if evidence.Capture.Review.Complete && evidence.Capture.Review.Outcome == "completed" {
		return attempt.State == "completed", nil
	}
	if attempt.State != "failed" || !usablePartialCapture(evidence.Capture) {
		return false, nil
	}
	operation, err := readOperation(ctx, q, evidence.ProjectID, evidence.OperationID)
	if err != nil || !usablePartialOperation(operation) {
		return false, err
	}
	proof, _, _, err := deriveComparisonProofFromOperation(ctx, q, evidence.ProjectID, evidence.OperationID, "2", operation)
	return proof.evidence == evidence.ID, err
}
