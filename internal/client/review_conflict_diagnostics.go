package client

// Only these typed guard facts may cross from a refusal into agent context.
// Provider text, proof bodies and arbitrary metadata never become diagnostics.
func safeReviewConflictDetails(value map[string]any) map[string]any {
	reason, _ := value["reason"].(string)
	field := ""
	switch reason {
	case "adjudication_revision_conflict":
		field = "expected_revision"
	case "fix_sha_conflict":
		field = "expected_sha"
	case "finding_not_accepted":
		field = "disposition"
	default:
		return nil
	}
	revision, ok := value["current_revision"].(float64)
	if !ok || revision < 0 || revision >= 1<<53 || revision != float64(int64(revision)) || value["field"] != field {
		return nil
	}
	out := map[string]any{"reason": reason, "field": field, "current_revision": int64(revision)}
	if reason == "adjudication_revision_conflict" {
		return out
	}
	finding, ok := value["finding"].(float64)
	sha, shaOK := value["current_fix_sha"].(string)
	disposition, dispositionOK := value["current_disposition"].(string)
	if !ok || finding < 0 || finding >= 100 || finding != float64(int64(finding)) || !shaOK || sha != "" && !controlHex(sha, 40) || !dispositionOK || !oneReviewOf(disposition, "unadjudicated", "accepted", "rejected", "routed") {
		return nil
	}
	out["finding"], out["current_fix_sha"], out["current_disposition"] = int64(finding), sha, disposition
	return out
}

func reviewConflictDiagnostic(result *Error) {
	result.Message = "The review record does not match the supplied guard."
	result.Action = "Read the current selected review record and correct the conflicting guard. Use a new idempotency key for a changed request; retain the original key and body for an unknown outcome."
	details, _ := result.Details.(map[string]any)
	switch details["reason"] {
	case "adjudication_revision_conflict":
		result.Message = "expected_revision does not match the current adjudication revision."
		result.Action = "Read the current adjudication and preserve concurrent changes. Submit the intended change with its current revision and a new idempotency key."
	case "fix_sha_conflict":
		result.Message = "expected_sha does not match this finding's currently recorded fix_sha."
		result.Action = "Use the finding's current fix_sha as expected_sha, or an empty string for its first fix; fix_sha names the new fix commit. The guard does not compare PR heads or reviewed commits. Recheck the adjudication revision and use a new key for the corrected request."
	case "finding_not_accepted":
		result.Message = "This finding is not accepted for direct fix recording."
		result.Action = "Read the current finding disposition. Decide it explicitly when authorized, or use the receiving route controls for routed work; then submit the intended fix with current guards and a new key."
	}
}
