package client

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReviewSourceRefusal retains one original comparison refusal. A missing parent
// carries its checked child/base branch, never invented parent identities.
type ReviewSourceRefusal struct {
	Cause             string `json:"cause,omitempty"`
	PullNumber        int64  `json:"pull_number"`
	ParentPullNumber  int64  `json:"parent_pull_number,omitempty"`
	ObservedBaseSHA   string `json:"observed_base_sha,omitempty"`
	ExpectedParentSHA string `json:"expected_parent_sha,omitempty"`
	BaseRef           string `json:"base_ref,omitempty"`
}

func validReviewSourceRefusal(v *ReviewSourceRefusal, state, reason string, selected int64) bool {
	if v == nil {
		return true
	}
	if state != "held" || !validPullNumber(v.PullNumber) {
		return false
	}
	switch reason {
	case "predecessor_unfinished":
		return v.PullNumber != selected && v.ParentPullNumber == 0 && v.ObservedBaseSHA == "" && v.ExpectedParentSHA == "" && v.BaseRef == "" && oneReviewOf(v.Cause, "not_in_queue", "discovery_incomplete", "incoming_route_pending")
	case "predecessor_parent_moved":
		return v.Cause == "" && v.BaseRef == "" && v.PullNumber != selected && validPullNumber(v.ParentPullNumber) && v.ParentPullNumber != v.PullNumber && controlHex(v.ObservedBaseSHA, 40) && controlHex(v.ExpectedParentSHA, 40) && v.ObservedBaseSHA != v.ExpectedParentSHA
	case "parent_unavailable":
		b, err := json.Marshal(v)
		return v.Cause == "" && v.ParentPullNumber == 0 && v.ObservedBaseSHA == "" && v.ExpectedParentSHA == "" && v.BaseRef != "" && len(v.BaseRef) <= 4096 && utf8.ValidString(v.BaseRef) && strings.IndexFunc(v.BaseRef, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0 && err == nil && len(b) <= 512
	}
	return false
}
