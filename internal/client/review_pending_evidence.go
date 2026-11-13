package client

import "strconv"

type ReviewPendingEvidence struct {
	EvaluatedAt string                      `json:"evaluated_at"`
	Items       []ReviewPendingEvidenceItem `json:"items"`
	HasMore     bool                        `json:"has_more"`
}
type ReviewPendingEvidenceItem struct {
	OperationID      string `json:"operation_id"`
	EvidenceID       string `json:"evidence_id,omitempty"`
	Finding          *int64 `json:"finding,omitempty"`
	RouteID          string `json:"route_id,omitempty"`
	TargetPullNumber int64  `json:"target_pull_number,omitempty"`
	Reason           string `json:"reason"`
}

func validReviewPendingEvidence(v *ReviewPendingEvidence, state, reason string) bool {
	if v == nil {
		return true
	}
	if state != "held" || reason != "review_evidence_pending" || !controlTime(v.EvaluatedAt) || v.Items == nil || len(v.Items) > 5 || v.HasMore && len(v.Items) != 5 {
		return false
	}
	seen := map[string]bool{}
	for _, x := range v.Items {
		if ValidateReviewOperationID(x.OperationID) != nil {
			return false
		}
		if x.Finding != nil && (*x.Finding < 0 || *x.Finding >= 100) {
			return false
		}
		switch x.Reason {
		case "capture_missing":
			if x.EvidenceID != "" || x.Finding != nil {
				return false
			}
		case "capture_incomplete", "finalization_pending":
			if ValidateReviewEvidenceID(x.EvidenceID) != nil || x.Finding != nil {
				return false
			}
		case "finding_undecided", "fix_pending":
			if ValidateReviewEvidenceID(x.EvidenceID) != nil || x.Finding == nil {
				return false
			}
		case "route_undecided", "route_fix_pending":
			if ValidateReviewEvidenceID(x.EvidenceID) != nil || x.Finding == nil || ValidateReviewRouteID(x.RouteID) != nil || !validPullNumber(x.TargetPullNumber) {
				return false
			}
		default:
			return false
		}
		if x.Reason != "route_undecided" && x.Reason != "route_fix_pending" && (x.RouteID != "" || x.TargetPullNumber != 0) {
			return false
		}
		key := x.OperationID + "/" + x.EvidenceID + "/" + x.Reason + "/" + x.RouteID
		if x.Finding != nil {
			key += "/" + strconv.FormatInt(*x.Finding, 10)
		}
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}
