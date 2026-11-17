package reviewprovider

import (
	"net/url"
	"testing"
)

func TestNextPageFixedCollectionSelectorsCannotChangeScope(t *testing.T) {
	path := "/repos/owner/repo/commits/0123456789abcdef0123456789abcdef01234567/check-runs"
	for _, suffix := range []string{"page=2&per_page=100&filter=all", "filter=all&per_page=100&page=2"} {
		page, exists, err := nextPageWithQuery([]string{"<https://api.github.com" + path + "?" + suffix + ">; rel=next"}, "https://api.github.com", path, 1, url.Values{"filter": {"all"}})
		if err != nil || !exists || page != 2 {
			t.Fatalf("valid fixed selector rejected: page=%d exists=%v err=%v", page, exists, err)
		}
	}
	for _, suffix := range []string{
		"page=3&per_page=100&filter=all", "page=2&per_page=100", "page=2&per_page=100&filter=latest", "page=2&per_page=100&filter=all&filter=all", "page=2&per_page=100&filter=all&app_id=1", "page=2&per_page=100&Filter=all",
	} {
		_, _, err := nextPageWithQuery([]string{"<https://api.github.com" + path + "?" + suffix + ">; rel=next"}, "https://api.github.com", path, 1, url.Values{"filter": {"all"}})
		if err == nil {
			t.Errorf("accepted changed scope: %s", suffix)
		}
	}
	for _, fixed := range []url.Values{{"page": {"2"}}, {"per_page": {"100"}}, {"filter": {"all", "all"}}, {"": {"all"}}, {"filter": {""}}} {
		if _, _, err := nextPageWithQuery(nil, "https://api.github.com", path, 1, fixed); err == nil {
			t.Errorf("accepted invalid caller selector: %v", fixed)
		}
	}
}

func TestNextPagePreservesExplicitEscapedBranchSelector(t *testing.T) {
	path := "/repos/owner/repo/rules/branches/topic%2Fchild"
	link := "<https://api.github.com" + path + "?page=2&per_page=100>; rel=next"
	page, exists, err := nextPage([]string{link}, "https://api.github.com", path, 1)
	if err != nil || !exists || page != 2 {
		t.Fatal("exact escaped branch pagination rejected", page, exists, err)
	}
	for _, target := range []string{
		"/repos/owner/repo/rules/branches/topic/child",
		"/repos/owner/repo/rules/branches/topic%252Fchild",
		"/repos/owner/repo/rules/branches/topic%2Fother",
		"/repos/owner/other/rules/branches/topic%2Fchild",
		"/repos/owner/repo/rules/branches/topic%2Fchild%2F..",
		"/repos/owner/repo/rules/branches/topic%2fchild",
	} {
		link := "<https://api.github.com" + target + "?page=2&per_page=100>; rel=next"
		if _, _, err := nextPage([]string{link}, "https://api.github.com", path, 1); err == nil {
			t.Fatalf("accepted changed branch selector: %s", target)
		}
	}
	// Existing plain selectors still cannot be replaced by an encoded alias.
	plain := "/repos/owner/repo/issues/1/comments"
	link = "<https://api.github.com/repos/owner/repo/issues/%31/comments?page=2&per_page=100>; rel=next"
	if _, _, err := nextPage([]string{link}, "https://api.github.com", plain, 1); err == nil {
		t.Fatal("plain collection accepted a newly encoded selector")
	}
}
