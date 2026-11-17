package reviewprovider

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type suiteQueryMetricsKey struct{}
type metadataQueryMetricsKey struct{}

func observedGitHubEndpoint(ctx context.Context, path string) string {
	if path == "/graphql" && ctx.Value(metadataQueryMetricsKey{}) == true {
		return "pull_metadata_graphql"
	}
	if path == "/graphql" && ctx.Value(suiteQueryMetricsKey{}) == true {
		return "suite_count_graphql"
	}
	return githubEndpoint(path)
}
func observedGitHubDetail(ctx context.Context, path string) string {
	if path == "/graphql" && ctx.Value(metadataQueryMetricsKey{}) == true {
		return "pull_metadata_graphql"
	}
	if path == "/graphql" && ctx.Value(suiteQueryMetricsKey{}) == true {
		return "suite_count_graphql"
	}
	return githubEndpointDetail(path)
}
func githubQuotaResource(header http.Header) string {
	switch resource := header.Get("X-RateLimit-Resource"); resource {
	case "core", "graphql", "search", "code_search", "integration_manifest", "dependency_snapshots", "code_scanning_upload", "actions_runner_registration", "scim", "audit_log":
		return resource
	}
	return "unknown"
}

// Cost is optional diagnostics, never input to CI validation, admission or retry.
// Limit diagnostic parsing independently of the existing larger response budget.
func suiteQueryCost(data []byte) (int64, bool) {
	if len(data) == 0 || len(data) > 64<<10 || strictjson.Validate(data) != nil {
		return 0, false
	}
	var body struct {
		Data *struct{ RateLimit *struct{ Cost *int64 } }
	}
	if json.Unmarshal(data, &body) != nil || body.Data == nil || body.Data.RateLimit == nil || body.Data.RateLimit.Cost == nil {
		return 0, false
	}
	cost := *body.Data.RateLimit.Cost
	return cost, cost >= 1 && cost <= 1_000_000_000
}

// Records completion-time samples in the existing fixed-cardinality quota rows.
// It adds no requests or log calls; a later ordinary quota flush reports them.
// Samples may cross header-accounting window boundaries and are not request totals.
func (q *GitHubQuota) observeSuiteQueryCost(ctx context.Context, header http.Header, data []byte) {
	if q == nil {
		return
	}
	cost, known := suiteQueryCost(data)
	caller, _ := ctx.Value(githubCallerKey{}).(string)
	if caller == "" {
		caller = "other"
	}
	key := githubQuotaKey{caller, observedGitHubEndpoint(ctx, "/graphql"), githubQuotaResource(header)}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.rows == nil {
		q.rows = make(map[githubQuotaKey]githubQuotaCount)
	}
	row, found := q.rows[key]
	if !found {
		row.remaining, row.reset, row.retryAfter = -1, -1, -1
	}
	if known {
		row.suiteCostKnown++
		row.suitePointsSum += uint64(cost)
		row.suitePointsMax = max(row.suitePointsMax, uint64(cost))
	} else {
		row.suiteCostUnknown++
	}
	q.rows[key] = row
}
