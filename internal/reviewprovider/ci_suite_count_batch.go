package reviewprovider

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/strictjson"
	"net/http"
	"strings"
	"time"
)

// This unconnected experimental seam batches only one count witness phase.
// Callers must issue a separate batch after inventory collection; this is not
// an atomic snapshot, full suite inventory, or permission to reuse a witness.
type ciSuiteCountResult struct {
	Count int64
	Err   error
}
type ciSuiteCountNode struct {
	Type   string `json:"__typename,required"`
	OID    string `json:"oid,required"`
	Suites struct {
		Total int64 `json:"totalCount,required"`
		Page  struct {
			Next bool `json:"hasNextPage,required"`
		} `json:"pageInfo,required"`
		Nodes []struct {
			ID     int64 `json:"databaseId,required"`
			Commit struct {
				OID string `json:"oid,required"`
			} `json:"commit,required"`
			App struct {
				ID int64 `json:"databaseId,required"`
			} `json:"app,required"`
		} `json:"nodes,required"`
	} `json:"checkSuites,required"`
}

const ciSuiteCountBatchQuery = `query($owner:String!,$repo:String!,$h0:GitObjectID!,$h1:GitObjectID!){repository(owner:$owner,name:$repo){nameWithOwner c0:object(oid:$h0){__typename ... on Commit{oid checkSuites(first:1){totalCount pageInfo{hasNextPage} nodes{databaseId commit{oid} app{databaseId}}}}} c1:object(oid:$h1){__typename ... on Commit{oid checkSuites(first:1){totalCount pageInfo{hasNextPage} nodes{databaseId commit{oid} app{databaseId}}}}}}}`

func (g *GitHub) ciSuiteCountsBatch(ctx context.Context, repository string, heads [2]string, budgets [2]*actionsBudget) (out [2]ciSuiteCountResult) {
	fail := func(err error) [2]ciSuiteCountResult { return [2]ciSuiteCountResult{{Err: err}, {Err: err}} }
	if ctx == nil || !validRepositorySelector(repository) {
		return fail(invalidSelector())
	}
	for i, h := range heads {
		if !validSHA(h) || strings.ToLower(h) != h || budgets[i] == nil {
			return fail(invalidSelector())
		}
	}
	if budgets[0] == budgets[1] {
		return fail(invalidSelector())
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return fail(providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return fail(providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false))
	}
	exhausted := [2]bool{budgets[0].pages >= githubMaxPages, budgets[1].pages >= githubMaxPages}
	if exhausted[0] || exhausted[1] {
		// A rejected member must not consume a query or poison a healthy one.
		// A singleton uses the original bounded REST count instead of querying
		// an unused alias. This is one request, never a batch followed by retry.
		for i := range heads {
			if exhausted[i] {
				out[i].Err = invalidResponse(false, http.StatusOK)
				continue
			}
			out[i].Count, out[i].Err = g.ciSuiteCount(ctx, "/repos/"+repository+"/commits/"+heads[i]+"/check-suites", heads[i], budgets[i])
			if ctx.Err() != nil {
				out[i] = ciSuiteCountResult{Err: providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)}
			}
		}
		return out
	}
	for _, b := range budgets {
		b.pages++
	}
	parts := strings.Split(repository, "/")
	body, _ := json.Marshal(map[string]any{"query": ciSuiteCountBatchQuery, "variables": map[string]string{"owner": parts[0], "repo": parts[1], "h0": heads[0], "h1": heads[1]}})
	data, headers, status, err := g.pullReviewPage(ctx, body)
	if err != nil {
		return fail(err)
	}
	if status != http.StatusOK {
		return fail(statusError(&http.Response{StatusCode: status, Header: headers}, false))
	}
	if strictjson.Validate(data) != nil {
		return fail(invalidResponse(false, status))
	}
	root, ok := jsonObject(data)
	if !ok {
		return fail(invalidResponse(false, status))
	}
	if raw, present := root["errors"]; present {
		var errs []json.RawMessage
		if json.Unmarshal(raw, &errs) != nil || errs == nil {
			return fail(invalidResponse(false, status))
		}
		if len(errs) > 0 {
			for _, entry := range errs {
				if _, ok := jsonObject(entry); !ok {
					return fail(invalidResponse(false, status))
				}
			}
			if failure := ciSuiteBatchPrimaryLimit(headers); failure != nil {
				return fail(failure)
			}
			// Unsupported secondary/body-only limits remain unclassified;
			// never infer a limit from a provider message substring.
			return fail(invalidResponse(false, status))
		}
	}
	d, ok := jsonObject(root["data"])
	if !ok {
		return fail(invalidResponse(false, status))
	}
	r, ok := jsonObject(d["repository"])
	if !ok {
		return fail(invalidResponse(false, status))
	}
	var name string
	if decodeProviderJSON(r["nameWithOwner"], &name) != nil || !strings.EqualFold(name, repository) {
		return fail(invalidResponse(false, status))
	}
	raws := [2]json.RawMessage{r["c0"], r["c1"]}
	if len(raws[0]) == 0 || len(raws[1]) == 0 {
		return fail(invalidResponse(false, status))
	}
	// Count each member's full raw fragment (including unknown fields) and the
	// shared envelope once per member, but not the other member's fragment.
	overhead := len(data) - len(raws[0]) - len(raws[1])
	for i, raw := range raws {
		budgets[i].bytes += overhead + len(raw)
		b := budgets[i]
		var node ciSuiteCountNode
		if b.bytes > githubMaxAggregate || decodeProviderJSON(raw, &node) != nil {
			out[i].Err = invalidResponse(false, status)
			continue
		}
		s := node.Suites
		b.items += len(s.Nodes)
		if node.Type != "Commit" || node.OID != heads[i] || s.Total < 0 || s.Total > githubMaxItems || s.Nodes == nil || len(s.Nodes) > 1 || (s.Total == 0) != (len(s.Nodes) == 0) || s.Page.Next != (s.Total > 1) || b.items > githubMaxItems {
			out[i].Err = invalidResponse(false, status)
			continue
		}
		for _, sample := range s.Nodes {
			if sample.ID <= 0 || sample.App.ID <= 0 || sample.Commit.OID != heads[i] {
				out[i].Err = invalidResponse(false, status)
			}
		}
		if out[i].Err != nil {
			continue
		}
		if s.Total >= 1000 {
			out[i].Err = ciInventoryIncomplete()
			continue
		}
		out[i].Count = s.Total
	}
	if ctx.Err() != nil {
		return fail(providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false))
	}
	return out
}

// HTTP 200 with errors can be a GraphQL primary-limit refusal. Preserve the
// actual status and exact bounded retry evidence; successful last-point data
// never calls this helper. This does not install a GraphQL quota gate.
func ciSuiteBatchPrimaryLimit(h http.Header) *Error {
	if len(h.Values("X-RateLimit-Resource")) != 1 || h.Get("X-RateLimit-Resource") != "graphql" || len(h.Values("X-RateLimit-Remaining")) != 1 || h.Get("X-RateLimit-Remaining") != "0" || len(h.Values("X-RateLimit-Reset")) != 1 {
		return nil
	}
	now := time.Now()
	timing := sourceRetryTiming(h, now)
	if timing.State != "proven" {
		return nil
	}
	failure := providerError("provider_rate_limited", "The provider rate limit was reached.", http.StatusOK, retryAfter(h, now), false)
	failure.RetryTiming = timing
	failure.retryDirected = true
	return failure
}
