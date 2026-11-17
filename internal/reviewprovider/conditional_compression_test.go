package reviewprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConditionalRepresentationBoundedAndCorruption(t *testing.T) {
	data := []byte(strings.Repeat("large varied stable inventory item ", 3000))
	encoded, compressed := encodeConditionalBody(data)
	if !compressed || len(encoded) > len(data)*7/8 {
		t.Fatal("not compressed", len(encoded))
	}
	e := conditionalEntry{body: encoded, compressed: true, expandedSize: len(data)}
	got, err := e.decodedBody()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("roundtrip", err)
	}
	cases := []conditionalEntry{e, e, e, e, e, e}
	cases[0].expandedSize--
	cases[1].expandedSize++
	cases[2].expandedSize = githubMaxBody + 1
	cases[3].body = bytes.Clone(encoded)
	cases[3].body[len(encoded)-1] ^= 1
	cases[4].body = append(bytes.Clone(encoded), 1)
	cases[5].body = encoded[:len(encoded)/2]
	for i, c := range cases {
		if _, err := c.decodedBody(); err == nil {
			t.Fatal("accepted corruption", i)
		}
	}
	small := []byte("{\"a\":1}")
	raw, c := encodeConditionalBody(small)
	small[0] = 'x'
	if c || string(raw) != "{\"a\":1}" {
		t.Fatal("small body alias/compression")
	}
	noise := make([]byte, 128<<10)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range noise {
		noise[i] = byte(r.Uint32())
	}
	raw, c = encodeConditionalBody(noise)
	if c || !bytes.Equal(raw, noise) {
		t.Fatal("incompressible body changed")
	}
}

type compressedPressureTransport struct {
	mu                sync.Mutex
	calls, full, same int
	body              []byte
	force             int
}

func (p *compressedPressureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	h := http.Header{"Etag": []string{"\"stable\""}}
	status := 200
	data := p.body
	if p.force != 0 {
		status = p.force
		data = nil
	} else if r.Header.Get("If-None-Match") == "\"stable\"" {
		status = 304
		data = nil
		p.same++
	} else {
		p.full++
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(data)), Request: r}, nil
}
func compressionFixtureBody(t *testing.T) []byte {
	t.Helper()
	rows := make([]map[string]any, 80)
	for i := range rows {
		rows[i] = map[string]any{"id": i, "author": fmt.Sprintf("synthetic-%d", i), "body": strings.Repeat(fmt.Sprintf("Section %d verifies an independently validated response with fresh authority and bounded storage. ", i), 48)}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestConditionalCompressedLargeWorkingSetRevalidatesEveryRead(t *testing.T) {
	body := compressionFixtureBody(t)
	p := &compressedPressureTransport{body: body}
	g := &GitHub{token: "synthetic", baseURL: githubAPIURL, httpClient: &http.Client{Transport: p}}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	for cycle := range 2 {
		for i := range 80 {
			got, _, status, err := g.requestBytes(context.Background(), "GET", fmt.Sprintf("/large/%d", i), nil, nil)
			if err != nil || status != 200 || !bytes.Equal(got, body) {
				t.Fatal(cycle, i, status, err)
			}
		}
	}
	runtime.ReadMemStats(&after)
	if p.calls != 160 || p.full != 80 || p.same != 80 || len(g.cache) != 80 || g.cacheBytes > conditionalMaxBytes {
		t.Fatal(p.calls, p.full, p.same, len(g.cache), g.cacheBytes)
	}
	total := 0
	for key, e := range g.cache {
		if !e.compressed {
			t.Fatal("large body not compressed")
		}
		total += conditionalSize(e) + len(key)
	}
	if total != g.cacheBytes {
		t.Fatal("incorrect byte accounting", total, g.cacheBytes)
	}
	t.Logf("body=%d raw-working-set=%d retained=%d requests=%d full=%d validated304=%d elapsed=%s total-allocated=%d heap-end=%d", len(body), 80*len(body), g.cacheBytes, p.calls, p.full, p.same, time.Since(start), after.TotalAlloc-before.TotalAlloc, after.HeapAlloc)
	p.force = 503
	if _, _, status, err := g.requestBytes(context.Background(), "GET", "/large/0", nil, nil); status != 503 || err != nil {
		t.Fatal(status, err)
	}
	p.force = 304
	foreign := &GitHub{token: "other-synthetic", baseURL: githubAPIURL, httpClient: &http.Client{Transport: p}}
	if _, _, _, err := foreign.requestBytes(context.Background(), "GET", "/large/0", nil, nil); err == nil {
		t.Fatal("foreign cache reused")
	}
	g.cacheMu.Lock()
	e := g.cache["https://api.github.com/large/0?"]
	e.expandedSize--
	g.cache["https://api.github.com/large/0?"] = e
	g.cacheMu.Unlock()
	if _, _, _, err := g.requestBytes(context.Background(), "GET", "/large/0", nil, nil); err == nil {
		t.Fatal("corrupt body reused")
	}
}
func TestConditionalCompressedTerminalPageAndConcurrentReads(t *testing.T) {
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = strings.Repeat(fmt.Sprintf("item-%d ", i), 100)
	}
	body, _ := json.Marshal(rows)
	p := &compressedPressureTransport{body: body}
	g := &GitHub{token: "synthetic", baseURL: githubAPIURL, httpClient: &http.Client{Transport: p}}
	q := url.Values{"page": {"1"}, "per_page": {"100"}}
	for range 2 {
		if _, _, _, err := g.requestBytes(context.Background(), "GET", "/terminal", q, nil); err != nil {
			t.Fatal(err)
		}
	}
	if p.full != 2 || p.same != 0 {
		t.Fatal("terminal full page used stale end", p.full, p.same)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 3 {
				got, _, _, err := g.requestBytes(context.Background(), "GET", fmt.Sprintf("/concurrent/%d", i), nil, nil)
				if err != nil || !bytes.Equal(got, body) {
					t.Error("concurrent body", err)
				}
			}
		})
	}
	wg.Wait()
	if g.cacheBytes > conditionalMaxBytes || len(g.cache) > conditionalMaxEntries {
		t.Fatal("limits")
	}
}
func BenchmarkConditionalCompressedRepresentation(b *testing.B) {
	data := []byte(strings.Repeat("one varied inventory payload with identifiers and text ", 7000))
	encoded, c := encodeConditionalBody(data)
	if !c {
		b.Fatal("fixture")
	}
	e := conditionalEntry{body: encoded, compressed: true, expandedSize: len(data)}
	b.Run("encode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			encodeConditionalBody(data)
		}
	})
	b.Run("decode304", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := e.decodedBody(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestConditionalCodecMaximumBodyResources(t *testing.T) {
	// Binary noise deliberately exercises the poorly-compressible codec path;
	// admission still requires valid JSON in production.
	noise := make([]byte, githubMaxBody)
	random := rand.New(rand.NewPCG(71, 93))
	for i := range noise {
		noise[i] = byte(random.Uint32())
	}
	pattern := []byte("bounded synthetic inventory item ")
	compressible := bytes.Repeat(pattern, githubMaxBody/len(pattern)+1)[:githubMaxBody]
	inputs := [][]byte{compressible, noise}
	for _, data := range inputs {
		start := time.Now()
		encoded, compressed := encodeConditionalBody(data)
		e := conditionalEntry{body: encoded, compressed: compressed, expandedSize: len(data)}
		decoded, err := e.decodedBody()
		if err != nil || !bytes.Equal(decoded, data) {
			t.Fatal(err)
		}
		t.Logf("maximum bytes=%d compressed=%t stored=%d encode-plus-decode=%s", len(data), compressed, len(encoded), time.Since(start))
	}
	start := time.Now()
	barrier := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(8)
	done.Add(8)
	for i := range 8 {
		go func() {
			defer done.Done()
			ready.Done()
			<-barrier
			data := inputs[i%2]
			encoded, compressed := encodeConditionalBody(data)
			e := conditionalEntry{body: encoded, compressed: compressed, expandedSize: len(data)}
			got, err := e.decodedBody()
			if err != nil || !bytes.Equal(got, data) {
				t.Error(err)
			}
		}()
	}
	ready.Wait()
	close(barrier)
	done.Wait()
	t.Logf("eight concurrent maximum bodies elapsed=%s; synchronous codec completion is the cancellation-latency upper work unit, not context-aware cancellation", time.Since(start))
}
