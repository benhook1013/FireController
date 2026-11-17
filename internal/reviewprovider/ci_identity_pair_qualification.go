package reviewprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CIIdentityPairQualification is diagnostic only: no candidate facts or authority.
type CIIdentityPairQualification struct {
	Bucket   string `json:"bucket,omitempty"`
	State    string `json:"state"`
	Requests int64  `json:"requests"`
}

const ciIdentityPairQuery = `query($owner:String!,$repo:String!,$n0:Int!,$n1:Int!){repository(owner:$owner,name:$repo){fullDatabaseId nameWithOwner p0:pullRequest(number:$n0){` + metadataPairFields + ` potentialMergeCommit{oid}} p1:pullRequest(number:$n1){` + metadataPairFields + ` potentialMergeCommit{oid}}} rateLimit{cost}}`

func decodeCIIdentityPair(data []byte, id int64, repository string, numbers [2]int64) (out [2]githubCIPull, err error) {
	bad := func() ([2]githubCIPull, error) { return [2]githubCIPull{}, invalidResponse(false, http.StatusOK) }
	// Reuse complete metadata validation, including root identity and partial errors.
	metadata := decodeMetadataPair(data, id, repository, numbers)
	for _, m := range metadata {
		if m.Err != nil {
			return out, m.Err
		}
	}
	var root struct {
		Data struct {
			Repository struct {
				P0 json.RawMessage `json:"p0,required"`
				P1 json.RawMessage `json:"p1,required"`
			} `json:"repository,required"`
		} `json:"data,required"`
	}
	// Metadata validation above already proved envelope shape and identities.
	// RawMessage is intentionally decoded only by encoding/json.
	if json.Unmarshal(data, &root) != nil {
		return bad()
	}
	for i, body := range []json.RawMessage{root.Data.Repository.P0, root.Data.Repository.P1} {
		var p metadataPairPull
		var extra struct {
			Potential *struct {
				OID string `json:"oid,required"`
			} `json:"potentialMergeCommit,required"`
		}
		if decodeProviderJSON(body, &p) != nil || decodeProviderJSON(body, &extra) != nil {
			return bad()
		}
		base, e := metadataRepo(p.BaseRepo)
		if e != nil {
			return bad()
		}
		raw := githubPull{Title: p.Title, Number: p.Number, Merged: p.Merged, ChangedFiles: p.Changed, Additions: &p.Additions, Deletions: &p.Deletions, Draft: &p.Draft, MergedAt: p.MergedAt, MergeState: p.MergeState}
		raw.State = metadata[i].Pull.State
		switch p.Mergeable {
		case "MERGEABLE":
			v := true
			raw.Mergeable = &v
		case "CONFLICTING":
			v := false
			raw.Mergeable = &v
		}
		raw.Base.SHA, raw.Base.Ref, raw.Base.Repo = p.Base, p.BaseRef, base
		raw.Head.SHA, raw.Head.Ref = p.Head, &p.HeadRef
		if p.HeadRepo != nil {
			head, e := metadataRepo(*p.HeadRepo)
			if e != nil {
				return bad()
			}
			raw.Head.Repo = &head
		}
		out[i] = githubCIPull{Pull: raw, Mergeable: raw.Mergeable}
		if extra.Potential != nil {
			out[i].MergeCommitSHA = &extra.Potential.OID
		}
		if !validCIPull(out[i], numbers[i]) {
			return bad()
		}
	}
	return out, nil
}
func stableCIIdentity(p githubCIPull) bool {
	return p.Pull.State == "open" && !p.Pull.Merged && p.Mergeable != nil && *p.Mergeable && p.MergeCommitSHA != nil && *p.MergeCommitSHA != p.Pull.Head.SHA && *p.MergeCommitSHA != p.Pull.Base.SHA
}

// QualifyCIIdentityPair brackets one paired test-merge read with ordinary REST
// candidate reads. It is deliberately unconnected: callers must hold the normal
// reservation and propagate provider errors to ordinary cooldown. A match is
// only a bounded, non-atomic open/unmerged sample, never CI publication proof.
func (g *GitHub) QualifyCIIdentityPair(ctx context.Context, id int64, repository string, numbers [2]int64) (result CIIdentityPairQualification, err error) {
	result.State = "refused"
	if ctx == nil || g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || id <= 0 || !validRepositorySelector(repository) || !validPullNumber(numbers[0]) || !validPullNumber(numbers[1]) || numbers[0] == numbers[1] || numbers[0] > 2147483647 || numbers[1] > 2147483647 {
		return result, invalidSelector()
	}
	if g.quota != nil {
		result.Bucket = g.quota.bucket
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, githubTargetContextKey{}, githubTargetKey{})
	endpoint, _ := url.Parse(g.baseURL)
	transport := g.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	bounded := &metadataQualificationTransport{base: transport, host: endpoint.Host, scheme: endpoint.Scheme}
	for i, n := range numbers {
		bounded.pullPaths[i] = "/repos/" + repository + "/pulls/" + strconv.FormatInt(n, 10)
	}
	client := *g.httpClient
	client.Transport = bounded
	probe := &GitHub{readGate: g.readGate, quota: g.quota, token: g.token, baseURL: g.baseURL, httpClient: &client}
	defer func() { result.Requests = bounded.calls.Load() }()
	budget := &actionsBudget{}
	read := func() (out [2]githubCIPull, err error) {
		for i := range numbers {
			if _, err = probe.actionsRead(ctx, bounded.pullPaths[i], nil, budget, &out[i]); err != nil {
				return out, err
			}
			if !validCIPull(out[i], numbers[i]) || out[i].Pull.Base.Repo.ID != id || !strings.EqualFold(out[i].Pull.Base.Repo.FullName, repository) {
				return out, invalidResponse(false, http.StatusOK)
			}
		}
		return out, nil
	}
	before, err := read()
	if err != nil {
		return result, err
	}
	for _, p := range before {
		if !stableCIIdentity(p) {
			result.State = "inconclusive"
			return result, nil
		}
	}
	parts := strings.Split(repository, "/")
	body, _ := json.Marshal(map[string]any{"query": ciIdentityPairQuery, "variables": map[string]any{"owner": parts[0], "repo": parts[1], "n0": numbers[0], "n1": numbers[1]}})
	data, headers, status, err := probe.boundedWitnessGraphQLPage(ctx, body, true)
	if err != nil {
		return result, err
	}
	if status != http.StatusOK {
		return result, statusError(&http.Response{StatusCode: status, Header: headers}, false)
	}
	paired, err := decodeCIIdentityPair(data, id, repository, numbers)
	if err != nil {
		// Preserve an actual GraphQL primary limit rather than hiding it as shape failure.
		if failure := ciSuiteBatchPrimaryLimit(headers); failure != nil {
			probe.quota.observeSuiteGraphQLPrimary(context.WithValue(ctx, metadataQueryMetricsKey{}, true), status, failure.RetryTiming)
			return result, failure
		}
		return result, err
	}
	after, err := read()
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, providerTransportError(ctx, ctx.Err(), false)
	}
	result.State = "inconclusive"
	for i := range numbers {
		if !stableCIIdentity(paired[i]) || !stableCIIdentity(after[i]) || !sameCIPull(before[i], paired[i]) || !sameCIPull(before[i], after[i]) {
			return result, nil
		}
	}
	result.State = "matched"
	return result, nil
}
