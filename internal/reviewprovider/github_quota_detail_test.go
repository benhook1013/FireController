package reviewprovider

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestGitHubRequestDetailsPartitionWithoutSelectors(t *testing.T) {
	q := NewGitHubQuota()
	ctx := WithGitHubCaller(context.Background(), "lifecycle_enrichment_periodic")
	now := time.Now()
	headers := http.Header{"X-Ratelimit-Resource": []string{"core"}}
	for i := 0; i < 1000; i++ {
		p := fmt.Sprintf("/repos/private/repo/actions/runs/%d/attempts/2/jobs", i)
		q.observe(ctx, now, p, &http.Response{StatusCode: 304, Header: headers}, true, false)
	}
	q.observe(ctx, now, "/repos/private/repo/actions/runs", &http.Response{StatusCode: 403, Header: headers}, false, false)
	q.observe(ctx, now, "/repos/private/repo/actions/runs/42", nil, false, false)
	q.observe(ctx, now, "/repos/private/repo/actions/runs/42", nil, false, true)
	total := 0
	labels := map[string]bool{}
	for _, row := range q.rows {
		for label, d := range row.details {
			labels[label] = true
			total += d.Requests
			if label == "actions_attempt_jobs" && (d.Requests != 1000 || d.NotModified != 1000) {
				t.Fatal(d)
			}
			if label == "actions_inventory" && d.Forbidden != 1 {
				t.Fatal(d)
			}
			if label == "actions_current_run" && (d.NoResponse != 1 || d.LocalSkips != 1 || d.Requests != 1) {
				t.Fatal(d)
			}
		}
	}
	if total != 1002 || len(labels) != 3 {
		t.Fatal(total, labels)
	}
}
func TestGitHubRequestDetailClassification(t *testing.T) {
	for p, w := range map[string]string{"/repos/o/r/commits/sha/check-suites": "check_suite_count", "/repos/o/r/commits/sha/check-runs": "check_inventory", "/repos/o/r/commits/sha/statuses": "status_inventory", "/repos/o/r/branches/feature%2Fname/protection": "branch_protection", "/repos/o/r/branches/feature%2Fname": "branch_tip", "/repos/o/r/rules/branches/main": "branch_rules", "/repos/o/r/git/commits/sha": "git_commit_identity", "/repos/o/r/branches/protection": "branch_tip", "/repos/o/r/branches/protection/protection": "branch_protection", "/graphql": "graphql", "/repositories/1": "repository_identity", "/user": "authenticated_identity", "/repos/o/r/pulls/2": "pull_identity", "/repos/o/r/pulls/2/reviews": "pull_reviews", "/repos/o/r/pulls/2/comments": "pull_comments", "/repos/o/r/issues/2/comments": "issue_comments", "/unknown/private": "other"} {
		if g := githubEndpointDetail(p); g != w {
			t.Fatal(p, g, w)
		}
	}
}
