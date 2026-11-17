package reviewprovider

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// Metadata, publication pages and CI inventories share this adapter's cache.
// Keep a larger small-response working set without increasing retained bytes.
const conditionalMaxEntries = 2048
const conditionalMaxBytes = 16 << 20

type conditionalEntry struct {
	etag             string
	body             []byte
	headers          http.Header
	used             uint64
	expandedSize     int
	compressed       bool
	fullTerminalPage bool
}

// A cached Link without rel=next describes an apparent end. On a full
// page that conclusion can become stale while the ETag remains unchanged.
func paginationContinues(headers []string) bool {
	for _, value := range headers {
		links, err := parseLinkHeader(value)
		if err != nil {
			return false
		}
		for _, link := range links {
			if link.next {
				return true
			}
		}
	}
	return false
}

// Cache scope is one authenticated adapter. It never substitutes for a server
// read: only a matching 304 permits reuse, including the original page links.
func (g *GitHub) conditionalRequest(request *http.Request) (conditionalEntry, bool) {
	if request.Method != http.MethodGet || g.token == "" {
		return conditionalEntry{}, false
	}
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	entry, ok := g.cache[request.URL.String()]
	if ok {
		g.recordConditionalLocked(request.URL.Path, "hit")
	} else {
		g.recordConditionalLocked(request.URL.Path, "miss")
	}
	if ok {
		g.cacheClock++
		entry.used = g.cacheClock
		g.cache[request.URL.String()] = entry
		request.Header.Set("If-None-Match", entry.etag)
	}
	return entry, ok
}

func conditionalSize(entry conditionalEntry) int {
	n := len(entry.etag) + len(entry.body)
	for k, values := range entry.headers {
		n += len(k)
		for _, v := range values {
			n += len(v)
		}
	}
	return n
}

func (g *GitHub) rememberConditional(request *http.Request, response *http.Response, data []byte) {
	if request.Method != http.MethodGet || g.token == "" || response.StatusCode != http.StatusOK {
		return
	}
	etag := response.Header.Get("ETag")
	// A syntactically invalid JSON response is never retained. Typed shape and
	// pagination validation still run for every fresh or revalidated response.
	cacheable := etag != "" && len(etag) <= 1024 && !strings.ContainsAny(etag, "\r\n") && !strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store") && strictjson.Validate(data) == nil
	entry := conditionalEntry{etag: etag, headers: response.Header.Clone()}
	bodyAllowed := len(data) <= githubMaxBody
	if cacheable && bodyAllowed {
		entry.body, entry.compressed = encodeConditionalBody(data)
		entry.expandedSize = len(data)
		if request.URL.Query().Has("page") && request.URL.Query().Get("per_page") == strconv.Itoa(githubPageSize) {
			var rows []json.RawMessage
			entry.fullTerminalPage = json.Unmarshal(data, &rows) == nil && len(rows) == githubPageSize && !paginationContinues(entry.headers.Values("Link"))
		}
	}
	size := conditionalSize(entry) + len(request.URL.String())
	if !cacheable || !bodyAllowed {
		size += len(data)
	}
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	key := request.URL.String()
	g.recordConditionalLocked(request.URL.Path, "response_200")
	if etag != "" {
		g.recordConditionalLocked(request.URL.Path, "etag_present")
	}
	if etag == "" {
		g.recordConditionalLocked(request.URL.Path, "no_etag")
	}
	if etag != "" && (len(etag) > 1024 || strings.ContainsAny(etag, "\r\n")) {
		g.recordConditionalLocked(request.URL.Path, "invalid_etag")
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store") {
		g.recordConditionalLocked(request.URL.Path, "no_store")
	}
	if !cacheable && etag != "" && len(etag) <= 1024 && !strings.ContainsAny(etag, "\r\n") && !strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store") {
		g.recordConditionalLocked(request.URL.Path, "invalid_json")
	}
	if size > conditionalMaxBytes {
		g.recordConditionalLocked(request.URL.Path, "oversize")
	}
	if old, ok := g.cache[key]; ok {
		g.cacheBytes -= conditionalSize(old) + len(key)
		delete(g.cache, key)
	}
	if !cacheable || !bodyAllowed || size > conditionalMaxBytes {
		return
	}
	for len(g.cache) >= conditionalMaxEntries || g.cacheBytes+size > conditionalMaxBytes {
		g.recordConditionalLocked(request.URL.Path, "eviction")
		if len(g.cache) >= conditionalMaxEntries {
			g.recordConditionalLocked(request.URL.Path, "entry_pressure")
		}
		if g.cacheBytes+size > conditionalMaxBytes {
			g.recordConditionalLocked(request.URL.Path, "byte_pressure")
		}
		var oldest string
		var clock uint64
		for k, candidate := range g.cache {
			if oldest == "" || candidate.used < clock {
				oldest, clock = k, candidate.used
			}
		}
		old := g.cache[oldest]
		g.cacheBytes -= conditionalSize(old) + len(oldest)
		delete(g.cache, oldest)
	}
	if g.cache == nil {
		g.cache = make(map[string]conditionalEntry)
	}
	g.cacheClock++
	entry.used = g.cacheClock
	g.cache[key] = entry
	g.cacheBytes += size
	g.recordConditionalLocked(request.URL.Path, "stored")
}
