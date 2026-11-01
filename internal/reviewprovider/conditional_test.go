package reviewprovider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConditionalGETRevalidatesAuthenticatedBodiesAndPageLinks(t *testing.T) {
	var calls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("credential missing")
		}
		calls.Add(1)
		etag := `"first"`
		if r.URL.Query().Get("page") == "2" {
			etag = `"second"`
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", etag)
		if r.URL.Query().Get("page") != "2" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/owner/repo/issues/1/comments?page=2&per_page=100>; rel="next"`, server.URL))
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	g := testGitHub(server, "secret")
	for range 2 {
		if _, err := g.IssueComments(context.Background(), "owner/repo", 1); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("every page must revalidate, including links retained through 304", calls.Load())
	}
	if len(g.cache) != 2 {
		t.Fatal("page cache", len(g.cache))
	}
}

func TestConditionalGETNeverUsesCacheOnErrorOrAcrossCredentials(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer other" || r.Header.Get("Authorization") == "" {
			if r.Header.Get("If-None-Match") != "" {
				t.Error("foreign credential cache used")
			}
		}
		if n == 2 {
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(429)
			return
		}
		if n == 3 {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"repo"`)
		_, _ = w.Write([]byte(`{"id":1,"full_name":"owner/repo","default_branch":"main"}`))
	}))
	defer server.Close()
	g := testGitHub(server, "secret")
	if _, err := g.Repository(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Repository(context.Background(), 1); !hasCode(err, "provider_rate_limited") {
		t.Fatal("cached body concealed provider error", err)
	}
	if _, err := g.Repository(context.Background(), 1); err != nil {
		t.Fatal("safe retry must revalidate original cached response", err)
	}
	for _, token := range []string{"other", ""} {
		if _, err := testGitHub(server, token).Repository(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 5 {
		t.Fatal(calls.Load())
	}
}

func TestConditionalGETBoundsConcurrentReadsAndInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"valid"`)
		if r.URL.Path == "/invalid" {
			_, _ = w.Write([]byte(`{"duplicate":1,"duplicate":2}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	g := testGitHub(server, "secret")
	var wg sync.WaitGroup
	for n := range 300 {
		wg.Go(func() {
			var out map[string]any
			if err := g.getJSON(context.Background(), fmt.Sprintf("/entry/%d", n), nil, &out); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var out map[string]any
	if err := g.getJSON(context.Background(), "/invalid", nil, &out); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	if len(g.cache) > conditionalMaxEntries || g.cacheBytes > conditionalMaxBytes {
		t.Fatal("unbounded cache", len(g.cache), g.cacheBytes)
	}
	for key := range g.cache {
		if strings.Contains(key, "invalid") {
			t.Fatal("invalid JSON retained")
		}
	}
}

func TestPullNullableMergeabilityRemainsExplicit(t *testing.T) {
	for _, v := range []struct{ wire, want string }{{"null", "unknown"}, {"true", "mergeable"}, {"false", "conflicting"}} {
		t.Run(v.wire, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"title":"Selected","number":1,"state":"open","merged":false,"mergeable":%s,"head":{"sha":"%s","repo":null},"base":{"sha":"%s","ref":"main","repo":{"id":1,"full_name":"owner/repo","default_branch":"main"}},"changed_files":0}`, v.wire, strings.Repeat("a", 40), strings.Repeat("b", 40))
			}))
			defer server.Close()
			p, err := testGitHub(server, "secret").Pull(context.Background(), "owner/repo", 1)
			if err != nil || p.Mergeability != v.want {
				t.Fatal(p, err)
			}
		})
	}
}

func TestPullRetainsDetailedMergeStateFromExistingRequest(t *testing.T) {
	for _, state := range []string{"clean", "blocked", "behind", "dirty", "draft", "has_hooks", "unstable", "unknown", "future-provider-state"} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprintf(w, `{"title":"Selected","number":1,"state":"open","merged":false,"mergeable":true,"mergeable_state":%q,"head":{"sha":"%s","repo":null},"base":{"sha":"%s","ref":"main","repo":{"id":1,"full_name":"owner/repo","default_branch":"main"}},"changed_files":0}`, state, strings.Repeat("a", 40), strings.Repeat("b", 40))
			}))
			defer server.Close()
			p, err := testGitHub(server, "secret").Pull(t.Context(), "owner/repo", 1)
			want := strings.ToUpper(state)
			if state == "future-provider-state" {
				want = "UNKNOWN"
			}
			if err != nil || p.MergeStateStatus != want || calls.Load() != 1 {
				t.Fatal(p, err, calls.Load())
			}
		})
	}
}

func TestConditionalGETByteBudgetAndMutationsAreNotCached(t *testing.T) {
	g := &GitHub{token: "secret"}
	body := []byte(`"` + strings.Repeat("x", 5<<20) + `"`)
	for n := range 4 {
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.github.com/entry/%d", n), nil)
		response := &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{`"large"`}}}
		g.rememberConditional(request, response, body)
	}
	if len(g.cache) != 3 || g.cacheBytes > conditionalMaxBytes {
		t.Fatal("body bytes did not evict least recently used entry", len(g.cache), g.cacheBytes)
	}
	before := len(g.cache)
	request, _ := http.NewRequest(http.MethodPost, "https://api.github.com/mutation", nil)
	g.rememberConditional(request, &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{`"mutation"`}}}, []byte(`{}`))
	if len(g.cache) != before {
		t.Fatal("mutation cached")
	}
}
