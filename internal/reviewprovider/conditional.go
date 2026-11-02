package reviewprovider

import (
	"net/http"
	"strings"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const conditionalMaxEntries = 256
const conditionalMaxBytes = 16 << 20

type conditionalEntry struct {
	etag    string
	body    []byte
	headers http.Header
	used    uint64
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
	entry := conditionalEntry{etag: etag, body: append([]byte(nil), data...), headers: response.Header.Clone()}
	size := conditionalSize(entry) + len(request.URL.String())
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	key := request.URL.String()
	if old, ok := g.cache[key]; ok {
		g.cacheBytes -= conditionalSize(old) + len(key)
		delete(g.cache, key)
	}
	if !cacheable || size > conditionalMaxBytes {
		return
	}
	for len(g.cache) >= conditionalMaxEntries || g.cacheBytes+size > conditionalMaxBytes {
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
}
