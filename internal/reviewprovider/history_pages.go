package reviewprovider

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

const historyMaxPage int64 = 2147483647

// ReviewsPage reads one explicitly selected page without fetching its continuation.
func (g *GitHub) ReviewsPage(ctx context.Context, repository string, number, page int64) (Page[HostedReview], error) {
	return historyPage(ctx, g, repository, number, page, "pulls", "reviews", normalizeHostedReview, func(v githubHostedReview) int64 { return v.ID })
}

// ReviewCommentsPage retains original review, reply, actor and location identities.
func (g *GitHub) ReviewCommentsPage(ctx context.Context, repository string, number, page int64) (Page[ReviewComment], error) {
	return historyPage(ctx, g, repository, number, page, "pulls", "comments", normalizeReviewComment, func(v githubReviewComment) int64 { return v.ID })
}

// IssueCommentsPage reads one explicitly selected issue-comment page.
func (g *GitHub) IssueCommentsPage(ctx context.Context, repository string, number, page int64) (Page[IssueComment], error) {
	return historyPage(ctx, g, repository, number, page, "issues", "comments", normalizeIssueComment, func(v githubIssueComment) int64 { return v.ID })
}

func historyPage[R, T any](ctx context.Context, g *GitHub, repository string, number, page int64, resource, collection string, normalize func(R) (T, error), identity func(R) int64) (Page[T], error) {
	if !validRepositorySelector(repository) || !validPullNumber(number) || page < 1 || page > historyMaxPage {
		return Page[T]{}, invalidSelector()
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return Page[T]{}, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	path := "/repos/" + repository + "/" + resource + "/" + strconv.FormatInt(number, 10) + "/" + collection
	query := url.Values{"page": {strconv.FormatInt(page, 10)}, "per_page": {strconv.Itoa(githubPageSize)}}
	data, headers, status, err := g.requestBytes(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return Page[T]{}, err
	}
	if status != http.StatusOK {
		return Page[T]{}, statusError(&http.Response{StatusCode: status, Header: headers}, false)
	}
	var raw []R
	if err := decodeProviderJSON(data, &raw); err != nil || raw == nil || len(raw) > githubPageSize {
		return Page[T]{}, invalidResponse(false, status)
	}
	next, _, err := nextPageWithLimit(headers.Values("Link"), g.baseURL, path, page, nil, historyMaxPage)
	if err != nil {
		return Page[T]{}, invalidResponse(false, status)
	}
	result := Page[T]{Items: make([]T, 0, len(raw)), NextPage: next}
	seen := make(map[int64]bool, len(raw))
	for _, row := range raw {
		item, err := normalize(row)
		id := identity(row)
		if err != nil || seen[id] {
			return Page[T]{}, invalidResponse(false, status)
		}
		seen[id] = true
		result.Items = append(result.Items, item)
	}
	return result, nil
}
