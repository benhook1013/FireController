package reviewprovider

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/strictjson"
	"net/http"
	"strconv"
	"strings"
)

const metadataPairFields = `title number state merged headRefOid headRefName headRepository{fullDatabaseId nameWithOwner defaultBranchRef{name}} baseRefOid baseRefName baseRepository{fullDatabaseId nameWithOwner defaultBranchRef{name}} changedFiles additions deletions isDraft mergedAt mergeable mergeStateStatus`
const metadataPairQuery = `query($owner:String!,$repo:String!,$n0:Int!,$n1:Int!){repository(owner:$owner,name:$repo){fullDatabaseId nameWithOwner p0:pullRequest(number:$n0){` + metadataPairFields + `} p1:pullRequest(number:$n1){` + metadataPairFields + `}} rateLimit{cost}}`

type metadataPairRepo struct {
	ID      string `json:"fullDatabaseId,required"`
	Name    string `json:"nameWithOwner,required"`
	Default *struct {
		Name string `json:"name,required"`
	} `json:"defaultBranchRef,required"`
}
type metadataPairPull struct {
	Title      string            `json:"title,required"`
	Number     int64             `json:"number,required"`
	State      string            `json:"state,required"`
	Merged     bool              `json:"merged,required"`
	Head       string            `json:"headRefOid,required"`
	HeadRef    string            `json:"headRefName,required"`
	HeadRepo   *metadataPairRepo `json:"headRepository,required"`
	Base       string            `json:"baseRefOid,required"`
	BaseRef    string            `json:"baseRefName,required"`
	BaseRepo   metadataPairRepo  `json:"baseRepository,required"`
	Changed    int64             `json:"changedFiles,required"`
	Additions  int64             `json:"additions,required"`
	Deletions  int64             `json:"deletions,required"`
	Draft      bool              `json:"isDraft,required"`
	MergedAt   *string           `json:"mergedAt,required"`
	Mergeable  string            `json:"mergeable,required"`
	MergeState string            `json:"mergeStateStatus,required"`
}
type PullMetadataPairResult struct {
	Pull       Pull
	Repository Repository
	Err        error
}

func metadataRepo(p metadataPairRepo) (githubRepository, error) {
	id, err := strconv.ParseInt(p.ID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != p.ID || p.Default == nil {
		return githubRepository{}, invalidResponse(false, http.StatusOK)
	}
	r := githubRepository{ID: id, FullName: p.Name, DefaultBranch: p.Default.Name}
	if _, err = normalizeRepository(r, id); err != nil {
		return githubRepository{}, invalidResponse(false, http.StatusOK)
	}
	return r, nil
}
func metadataPull(p metadataPairPull, id int64, name string, number int64) (out PullMetadataPairResult) {
	fail := func() PullMetadataPairResult {
		return PullMetadataPairResult{Err: invalidResponse(false, http.StatusOK)}
	}
	base, err := metadataRepo(p.BaseRepo)
	if err != nil || base.ID != id || !strings.EqualFold(base.FullName, name) {
		return fail()
	}
	raw := githubPull{Title: p.Title, Number: p.Number, Merged: p.Merged, ChangedFiles: p.Changed, Additions: &p.Additions, Deletions: &p.Deletions, Draft: &p.Draft, MergedAt: p.MergedAt, MergeState: p.MergeState}
	switch p.State {
	case "OPEN":
		raw.State = "open"
	case "CLOSED", "MERGED":
		raw.State = "closed"
	default:
		return fail()
	}
	if p.Merged != (p.State == "MERGED") {
		return fail()
	}
	switch p.Mergeable {
	case "UNKNOWN":
	case "MERGEABLE":
		v := true
		raw.Mergeable = &v
	case "CONFLICTING":
		v := false
		raw.Mergeable = &v
	default:
		return fail()
	}
	switch p.MergeState {
	case "BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE":
	default:
		return fail()
	}
	raw.Base.SHA, raw.Base.Ref, raw.Base.Repo = p.Base, p.BaseRef, base
	raw.Head.SHA, raw.Head.Ref = p.Head, &p.HeadRef
	if p.HeadRepo != nil {
		head, e := metadataRepo(*p.HeadRepo)
		if e != nil {
			return fail()
		}
		raw.Head.Repo = &head
	}
	out.Pull, err = normalizePull(raw, number)
	if err != nil {
		return fail()
	}
	out.Repository, err = normalizeRepository(base, id)
	if err != nil {
		return fail()
	}
	return out
}
func decodeMetadataPair(body []byte, id int64, name string, numbers [2]int64) (out [2]PullMetadataPairResult) {
	fail := func() [2]PullMetadataPairResult {
		e := invalidResponse(false, http.StatusOK)
		return [2]PullMetadataPairResult{{Err: e}, {Err: e}}
	}
	if id <= 0 || !validRepositorySelector(name) || !validPullNumber(numbers[0]) || !validPullNumber(numbers[1]) || numbers[0] == numbers[1] || len(body) > 64<<10 {
		return fail()
	}
	var wire struct {
		Data struct {
			Repository struct {
				ID   string            `json:"fullDatabaseId,required"`
				Name string            `json:"nameWithOwner,required"`
				P0   *metadataPairPull `json:"p0,required"`
				P1   *metadataPairPull `json:"p1,required"`
			} `json:"repository,required"`
		} `json:"data,required"`
		Errors []json.RawMessage `json:"errors"`
	}
	if decodeProviderJSON(body, &wire) != nil || len(wire.Errors) > 0 {
		return fail()
	}
	r := wire.Data.Repository
	if r.ID != strconv.FormatInt(id, 10) || !strings.EqualFold(r.Name, name) {
		return fail()
	}
	for i, p := range []*metadataPairPull{r.P0, r.P1} {
		if p == nil {
			out[i].Err = invalidResponse(false, http.StatusOK)
		} else {
			out[i] = metadataPull(*p, id, name, numbers[i])
		}
	}
	return out
}

// PullMetadataPair reads two selected PRs in one bounded GraphQL request. It is
// not an atomic snapshot, CI proof or discovery API. Its scheduler option defaults off.
func (g *GitHub) PullMetadataPair(ctx context.Context, id int64, repository string, numbers [2]int64) (out [2]PullMetadataPairResult) {
	fail := func(err error) [2]PullMetadataPairResult { return [2]PullMetadataPairResult{{Err: err}, {Err: err}} }
	if ctx == nil || id <= 0 || !validRepositorySelector(repository) || !validPullNumber(numbers[0]) || !validPullNumber(numbers[1]) || numbers[0] == numbers[1] || numbers[0] > 2147483647 || numbers[1] > 2147483647 {
		return fail(invalidSelector())
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return fail(providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false))
	}
	ctx, cancel := context.WithTimeout(ctx, githubRequestLimit)
	defer cancel()
	parts := strings.Split(repository, "/")
	body, _ := json.Marshal(map[string]any{"query": metadataPairQuery, "variables": map[string]any{"owner": parts[0], "repo": parts[1], "n0": numbers[0], "n1": numbers[1]}})
	data, headers, status, err := g.boundedWitnessGraphQLPage(ctx, body, true)
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
		for _, entry := range errs {
			if _, ok := jsonObject(entry); !ok {
				return fail(invalidResponse(false, status))
			}
		}
		if len(errs) > 0 {
			if failure := ciSuiteBatchPrimaryLimit(headers); failure != nil {
				g.quota.observeSuiteGraphQLPrimary(context.WithValue(ctx, metadataQueryMetricsKey{}, true), http.StatusOK, failure.RetryTiming)
				return fail(failure)
			}
			return fail(invalidResponse(false, status))
		}
	}
	if ctx.Err() != nil {
		return fail(providerTransportError(ctx, ctx.Err(), false))
	}
	return decodeMetadataPair(data, id, repository, numbers)
}
