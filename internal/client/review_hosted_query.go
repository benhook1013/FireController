package client

import (
	"net/url"
	"strconv"
	"strings"
)

type ReviewHostedQueryDiagnostic struct {
	QueryID        string `json:"query_id"`
	QueryCommentID int64  `json:"query_comment_id,omitempty"`
	QueryURL       string `json:"query_url,omitempty"`
	ObservationID  string `json:"observation_id,omitempty"`
	CaptureID      string `json:"capture_id,omitempty"`
	CapturedAt     string `json:"captured_at,omitempty"`
}

func validReviewHostedQuery(v *ReviewHostedQueryDiagnostic, operation ReviewOperation) bool {
	if v == nil {
		return true
	}
	if operation.Channel != "hosted" || operation.State != "held" || operation.AttemptID == "" || !oneReviewOf(operation.Reason, "hosted_allowance_unproven", "hosted_allowance_reply_pending", "hosted_allowance_cooldown") || !validReviewOpaqueID(v.QueryID, "review_hosted_query_") || v.QueryCommentID < 0 || (v.QueryCommentID == 0) != (v.QueryURL == "") {
		return false
	}
	if v.QueryURL != "" {
		u, err := url.Parse(v.QueryURL)
		if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" || u.RawPath != "" || u.RawFragment != "" || u.Fragment != "issuecomment-"+strconv.FormatInt(v.QueryCommentID, 10) {
			return false
		}
		parts := strings.Split(u.Path, "/")
		if len(parts) != 5 || parts[0] != "" || !hostedQueryRepositoryPart(parts[1]) || !hostedQueryRepositoryPart(parts[2]) || parts[3] != "pull" || parts[4] != strconv.FormatInt(operation.PullNumber, 10) {
			return false
		}
	}
	if v.ObservationID == "" && v.CaptureID == "" && v.CapturedAt == "" {
		return true
	}
	return validReviewOpaqueID(v.ObservationID, "review_hosted_observation_") && validReviewOpaqueID(v.CaptureID, "review_capture_") && controlTime(v.CapturedAt)
}

func hostedQueryRepositoryPart(value string) bool {
	if value == "" || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
