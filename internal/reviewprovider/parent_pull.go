package reviewprovider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrParentAmbiguous means the bounded parent inventory cannot prove uniqueness.
// Callers may retain an explicit hold without exposing provider response details.
var ErrParentAmbiguous = errors.New("the published parent is ambiguous")

type githubParentRepository struct {
	ID       int64  `json:"id,required"`
	FullName string `json:"full_name,required"`
}
type githubParentPull struct {
	Number int64  `json:"number,required"`
	State  string `json:"state,required"`
	Head   struct {
		Ref  string                  `json:"ref,required"`
		Repo *githubParentRepository `json:"repo,required"`
	} `json:"head,required"`
	Base struct {
		Repo githubParentRepository `json:"repo,required"`
	} `json:"base,required"`
}

// ParentPull resolves an actual open same-repository pull whose head is the
// selected base branch. It reads one bounded candidate page and freshly reads
// the unique pull; queue order and historical source captures play no role.
func (g *GitHub) ParentPull(ctx context.Context, repository, baseRef string) (Pull, error) {
	if !validRepositorySelector(repository) || !validRequiredBranch(baseRef) {
		return Pull{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return Pull{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	owner := strings.SplitN(repository, "/", 2)[0]
	query := url.Values{"state": {"open"}, "head": {owner + ":" + baseRef}, "per_page": {"2"}, "page": {"1"}}
	path := "/repos/" + repository + "/pulls"
	body, headers, status, err := g.requestBytes(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return Pull{}, err
	}
	if status != http.StatusOK {
		return Pull{}, statusError(&http.Response{StatusCode: status, Header: headers}, false)
	}
	var candidates []githubParentPull
	// Any pagination leaves uniqueness unproven. Never follow a second page or
	// infer absence from an incomplete or malformed candidate inventory.
	if decodeProviderJSON(body, &candidates) != nil || candidates == nil || len(candidates) > 2 {
		return Pull{}, invalidResponse(false, status)
	}
	seen := map[int64]bool{}
	for _, candidate := range candidates {
		if !validPullNumber(candidate.Number) || seen[candidate.Number] || candidate.State != "open" || candidate.Head.Ref != baseRef || candidate.Head.Repo == nil || candidate.Base.Repo.ID <= 0 || candidate.Head.Repo.ID != candidate.Base.Repo.ID || !validRepositorySelector(candidate.Head.Repo.FullName) || !validRepositorySelector(candidate.Base.Repo.FullName) || !strings.EqualFold(candidate.Head.Repo.FullName, repository) || !strings.EqualFold(candidate.Base.Repo.FullName, repository) {
			return Pull{}, invalidResponse(false, status)
		}
		seen[candidate.Number] = true
	}
	// Validate the existing pagination framing/scope, but never follow it.
	// Malformed links remain provider errors; a valid partial page cannot prove uniqueness.
	for _, header := range headers.Values("Link") {
		links, err := parseLinkHeader(header)
		if err != nil || len(links) == 0 {
			return Pull{}, invalidResponse(false, status)
		}
		base, _ := url.Parse(g.baseURL)
		for _, link := range links {
			target, err := url.Parse(link.target)
			if err != nil || target.Scheme != base.Scheme || !strings.EqualFold(target.Host, base.Host) || target.User != nil || target.Fragment != "" || !validPaginationPath(target, strings.TrimRight(base.Path, "/"), path) {
				return Pull{}, invalidResponse(false, status)
			}
			nextQuery, err := url.ParseQuery(target.RawQuery)
			if err != nil || len(nextQuery) != len(query) {
				return Pull{}, invalidResponse(false, status)
			}
			for key, values := range query {
				if len(nextQuery[key]) != 1 || key != "page" && nextQuery.Get(key) != values[0] {
					return Pull{}, invalidResponse(false, status)
				}
			}
			page, err := strconv.ParseInt(nextQuery.Get("page"), 10, 64)
			if err != nil || page < 2 || strconv.FormatInt(page, 10) != nextQuery.Get("page") {
				return Pull{}, invalidResponse(false, status)
			}
		}
	}
	if len(candidates) > 1 || len(headers.Values("Link")) > 0 {
		return Pull{}, ErrParentAmbiguous
	}
	if len(candidates) == 0 {
		return Pull{}, nil
	}
	candidate := candidates[0]
	pull, raw, err := g.pullWithIdentity(ctx, repository, candidate.Number)
	if err != nil {
		return Pull{}, err
	}
	if pull.State != "open" || pull.Merged || pull.RepositoryID != candidate.Base.Repo.ID || pull.HeadRef != baseRef || raw.Head.Repo == nil || raw.Head.Repo.ID != pull.RepositoryID || !strings.EqualFold(raw.Head.Repo.FullName, repository) || !strings.EqualFold(raw.Base.Repo.FullName, repository) {
		return Pull{}, invalidResponse(false, http.StatusOK)
	}
	return pull, nil
}
