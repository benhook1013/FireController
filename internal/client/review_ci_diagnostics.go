package client

import (
	"strings"
	"unicode"
)

type ReviewCIContext struct {
	Context      string                `json:"context"`
	AppID        int64                 `json:"app_id"`
	State        string                `json:"state"`
	Reason       string                `json:"reason"`
	CheckRun     *ReviewCICheckRun     `json:"check_run,omitempty"`
	CommitStatus *ReviewCICommitStatus `json:"commit_status,omitempty"`
}

type ReviewCICheckRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ReviewCICommitStatus struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}

func validCIContextDetails(v ReviewSourceCIAssessment) bool {
	if v.Checks == nil {
		return true
	}
	checks := *v.Checks
	if checks == nil || len(checks) > 128 || int64(len(checks)) != v.Required {
		return false
	}
	counts := map[string]int64{}
	seen := map[struct {
		context string
		app     int64
	}]bool{}
	text := func(s string, max int, required bool) bool {
		return controlText(s, max, required) && strings.IndexFunc(s, unicode.IsControl) < 0
	}
	for _, c := range checks {
		key := struct {
			context string
			app     int64
		}{c.Context, c.AppID}
		if !text(c.Context, 255, true) || c.AppID < 0 || seen[key] || !oneReviewOf(c.State, "satisfied", "failed", "pending", "unproven") || c.State == "satisfied" && c.Reason != "" || c.State != "satisfied" && !validExecutionIdentifier(c.Reason) {
			return false
		}
		seen[key] = true
		counts[c.State]++
		if c.CheckRun != nil && (c.CheckRun.ID < 1 || !text(c.CheckRun.Status, 64, true) || !text(c.CheckRun.Conclusion, 64, false)) {
			return false
		}
		if c.CommitStatus != nil && (c.CommitStatus.ID < 1 || !text(c.CommitStatus.State, 64, true)) {
			return false
		}
	}
	return counts["satisfied"] == v.Satisfied && counts["failed"] == v.Failed && counts["pending"] == v.Pending && counts["unproven"] == v.Unproven
}
