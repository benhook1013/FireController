package reviewprovider

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The pressure fixture returns a stable ETag for each exact URL. Hits still
// contact this transport and receive 304; misses receive a fresh JSON body.
// It measures revalidation eligibility, not live GitHub quota or scheduling.
type conditionalPressureTransport struct {
	mu         sync.Mutex
	full, same int
}

func (p *conditionalPressureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	status, body := http.StatusOK, `{}`
	etag := strconv.Quote(r.URL.String())
	headers := http.Header{"Etag": []string{etag}}
	if r.Header.Get("If-None-Match") == etag {
		status, body = http.StatusNotModified, ""
		p.same++
	} else {
		p.full++
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func (p *conditionalPressureTransport) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.full, p.same
}

func newConditionalPressureClient() (*GitHub, *conditionalPressureTransport) {
	p := &conditionalPressureTransport{}
	return &GitHub{token: "synthetic-cache-pressure", baseURL: githubAPIURL, httpClient: &http.Client{Transport: p}}, p
}

// Derive the CI request trace from the real collector and existing valid wire
// fixture, rather than independently reproducing its validation fanout. The
// remaining replay uses small synthetic bodies to isolate the entry ceiling.
func conditionalPressureCITrace(t *testing.T) []string {
	t.Helper()
	f := newCIObservationFixture(t, "merge")
	value, err := f.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	if err != nil || value.State != "satisfied" {
		t.Fatal("CI trace did not prove its selected gate", value.State, err)
	}
	paths, _ := f.snapshot()
	if len(paths) != 28 {
		t.Fatalf("one-workflow green CI reads = %d, want 28; review pressure assumptions", len(paths))
	}
	return paths
}

func conditionalPressureTargetURI(t *testing.T, raw string, number int) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	merge := fmt.Sprintf("%040x", 100000+number)
	u.Path = strings.ReplaceAll(u.Path, candidateMerge, merge)
	if u.Path == candidatePullPath {
		u.Path = "/repos/owner/repo/pulls/" + strconv.Itoa(number)
	}
	u.Path = strings.Replace(u.Path, actionsRunsPath+"/50", actionsRunsPath+"/"+strconv.Itoa(10000+number), 1)
	query := u.Query()
	if query.Get("head_sha") == candidateMerge {
		query.Set("head_sha", merge)
	}
	if query.Get("check_suite_id") == "40" {
		query.Set("check_suite_id", strconv.Itoa(20000+number))
	}
	u.RawQuery = query.Encode()
	return u.RequestURI()
}

func TestConditionalGETRetainsEnrolledSweepWorkingSet(t *testing.T) {
	ciTrace := conditionalPressureCITrace(t)
	for _, number := range []int{8, 36, 84, 100} {
		t.Run(strconv.Itoa(number), func(t *testing.T) {
			g, transport := newConditionalPressureClient()
			seen := map[string]bool{}
			read := func(raw string) {
				t.Helper()
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				// The adapter always builds path + "?" + encoded query, including
				// the empty query used by metadata and candidate reads alike.
				seen[u.Path+"?"+u.Query().Encode()] = true
				var body map[string]any
				if err := g.getJSON(t.Context(), u.Path, u.Query(), &body); err != nil {
					t.Fatal(err)
				}
			}
			metadata := func(pull int) {
				read("/repositories/20")
				read("/repos/owner/repo/pulls/" + strconv.Itoa(pull))
			}
			var warmFull, warmSame int
			for cycle := range 10 {
				if cycle == 5 {
					warmFull, warmSame = transport.counts()
				}
				// One permitted lane ordering: metadata sweep, then enrichment.
				// Each fifth enrichment includes a full green CI collection.
				for pull := 1; pull <= number; pull++ {
					metadata(pull)
				}
				for pull := 1; pull <= number; pull++ {
					metadata(pull)
					if cycle%5 == 0 {
						for _, raw := range ciTrace {
							read(conditionalPressureTargetURI(t, raw, pull))
						}
						read("/repositories/20") // Final gate selection recheck.
					}
					for _, path := range []string{"pulls/%d/reviews", "pulls/%d/comments", "issues/%d/comments"} {
						read("/repos/owner/repo/" + fmt.Sprintf(path, pull) + "?page=1&per_page=100")
					}
				}
			}
			full, same := transport.counts()
			t.Logf("keys=%d, retained=%d, warmed five-sweep full responses=%d, conditional 304=%d", len(seen), len(g.cache), full-warmFull, same-warmSame)
			if len(seen) != 4+12*number {
				t.Fatalf("distinct cache keys = %d, want %d; review pressure assumptions", len(seen), 4+12*number)
			}
			if len(g.cache) != len(seen) {
				t.Fatalf("small-body working set did not fit: retained=%d, keys=%d", len(g.cache), len(seen))
			}
			if full != warmFull || same-warmSame != 64*number {
				t.Fatalf("stable warm sweep: full responses=%d, conditional 304=%d, want 0/%d; keys=%d", full-warmFull, same-warmSame, 64*number, len(seen))
			}
			t.Logf("keys=%d, warmed five-sweep reads=%d, full responses=0; every read revalidated", len(seen), same-warmSame)
		})
	}
}

func TestConditionalGETExactEntryLimitAndLRUTouch(t *testing.T) {
	g := &GitHub{token: "synthetic-entry-limit"}
	request := func(n int) *http.Request {
		r, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.github.com/entry/%04d", n), nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": []string{`"stable"`}}}
	for n := range conditionalMaxEntries {
		g.rememberConditional(request(n), response, []byte(`{}`))
	}
	if len(g.cache) != conditionalMaxEntries {
		t.Fatal("entry ceiling was not filled", len(g.cache))
	}
	if _, ok := g.conditionalRequest(request(0)); !ok {
		t.Fatal("least recently used entry disappeared before capacity")
	}
	g.rememberConditional(request(conditionalMaxEntries), response, []byte(`{}`))
	if len(g.cache) != conditionalMaxEntries {
		t.Fatal("entry limit changed", len(g.cache))
	}
	if _, ok := g.conditionalRequest(request(1)); ok {
		t.Fatal("insertion did not evict the least recently used entry")
	}
	if _, ok := g.conditionalRequest(request(0)); !ok {
		t.Fatal("conditional revalidation did not protect the touched entry")
	}
}

func TestConditionalGETExactByteLimitAndLRUTouch(t *testing.T) {
	g := &GitHub{token: "synthetic-byte-limit"}
	request := func(n int) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://api.github.com/entry/%04d", n), nil)
		return r
	}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Etag": []string{`"stable"`}}}
	overhead := conditionalSize(conditionalEntry{etag: `"stable"`, headers: response.Header}) + len(request(0).URL.String())
	body := []byte(`"` + strings.Repeat("x", conditionalMaxBytes/4-overhead-2) + `"`)
	for n := range 4 {
		g.rememberConditional(request(n), response, body)
	}
	if g.cacheBytes != conditionalMaxBytes || len(g.cache) != 4 {
		t.Fatal("exact byte ceiling did not fit", g.cacheBytes, len(g.cache))
	}
	_, _ = g.conditionalRequest(request(0))
	g.rememberConditional(request(4), response, body)
	if g.cacheBytes != conditionalMaxBytes || len(g.cache) != 4 {
		t.Fatal("byte ceiling did not bound replacement", g.cacheBytes, len(g.cache))
	}
	if _, ok := g.conditionalRequest(request(1)); ok {
		t.Fatal("byte pressure retained the oldest entry")
	}
	if _, ok := g.conditionalRequest(request(0)); !ok {
		t.Fatal("byte pressure evicted the recently revalidated entry")
	}
}

func TestConditionalGETConcurrentPressureRemainsBounded(t *testing.T) {
	g, transport := newConditionalPressureClient()
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Go(func() {
			for n := range conditionalMaxEntries/8 + 8 {
				path := fmt.Sprintf("/entry/%d/%d", worker, n)
				for range 2 {
					var body map[string]any
					if err := g.getJSON(t.Context(), path, nil, &body); err != nil {
						t.Error(err)
					}
				}
			}
		})
	}
	workers.Wait()
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	if len(g.cache) != conditionalMaxEntries || g.cacheBytes > conditionalMaxBytes {
		t.Fatal("concurrent pressure did not retain bounded entries", len(g.cache), g.cacheBytes)
	}
	full, same := transport.counts()
	if full+same != 16*(conditionalMaxEntries/8+8) || same == 0 {
		t.Fatal("concurrent requests skipped network revalidation", full, same)
	}
}
