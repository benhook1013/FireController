package reviewprovider

import (
	"net/http"
	"strings"
)

// Fixed labels only: never retain provider paths, selectors or response bodies.
// Details partition existing network totals, not additional provider requests.
type githubRequestDetail struct {
	Requests        int `json:"requests"`
	NotModified     int `json:"not_modified"`
	Forbidden       int `json:"forbidden"`
	TooManyRequests int `json:"too_many_requests"`
	NoResponse      int `json:"no_response"`
	LocalSkips      int `json:"local_skips"`
}

func (d *githubRequestDetail) add(status int, conditional, gated, noResponse bool) {
	if gated {
		d.LocalSkips++
		return
	}
	d.Requests++
	if status == http.StatusNotModified && conditional {
		d.NotModified++
	}
	if status == http.StatusForbidden {
		d.Forbidden++
	}
	if status == http.StatusTooManyRequests {
		d.TooManyRequests++
	}
	if noResponse {
		d.NoResponse++
	}
}
func githubEndpointDetail(path string) string {
	p := strings.Split(strings.Trim(path, "/"), "/")
	if path == "/graphql" {
		return "graphql"
	}
	if path == "/user" {
		return "authenticated_identity"
	}
	if len(p) == 2 && p[0] == "repositories" {
		return "repository_identity"
	}
	if len(p) < 4 || p[0] != "repos" {
		return "other"
	}
	switch p[3] {
	case "actions":
		if len(p) == 5 && p[4] == "runs" {
			return "actions_inventory"
		}
		if len(p) == 6 && p[4] == "runs" {
			return "actions_current_run"
		}
		if len(p) == 9 && p[4] == "runs" && p[6] == "attempts" && p[8] == "jobs" {
			return "actions_attempt_jobs"
		}
	case "commits":
		if len(p) == 6 {
			switch p[5] {
			case "check-suites":
				return "check_suite_count"
			case "check-runs":
				return "check_inventory"
			case "statuses":
				return "status_inventory"
			}
		}
	case "branches":
		if len(p) == 6 && p[5] == "protection" {
			return "branch_protection"
		}
		if len(p) == 5 {
			return "branch_tip"
		}
	case "rules":
		return "branch_rules"
	case "git":
		if len(p) == 6 && p[4] == "commits" {
			return "git_commit_identity"
		}
	case "pulls":
		if len(p) == 5 {
			return "pull_identity"
		}
		if len(p) == 6 {
			switch p[5] {
			case "reviews":
				return "pull_reviews"
			case "comments":
				return "pull_comments"
			}
		}
	case "issues":
		if len(p) == 6 && p[5] == "comments" {
			return "issue_comments"
		}
	}
	return "other"
}
