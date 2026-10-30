package client

// ReviewSourceRefusal describes the original mismatched ancestor comparison.
type ReviewSourceRefusal struct {
	PullNumber        int64  `json:"pull_number"`
	ParentPullNumber  int64  `json:"parent_pull_number"`
	ObservedBaseSHA   string `json:"observed_base_sha"`
	ExpectedParentSHA string `json:"expected_parent_sha"`
}

func validReviewSourceRefusal(v *ReviewSourceRefusal, state, reason string, selected int64) bool {
	return v == nil || state == "held" && reason == "predecessor_parent_moved" && validPullNumber(v.PullNumber) && v.PullNumber != selected && validPullNumber(v.ParentPullNumber) && v.ParentPullNumber != v.PullNumber && controlHex(v.ObservedBaseSHA, 40) && controlHex(v.ExpectedParentSHA, 40) && v.ObservedBaseSHA != v.ExpectedParentSHA
}
