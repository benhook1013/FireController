package webui

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func browserFixture() http.Handler {
	return handlerFS(fstest.MapFS{
		"index.html":                   {Data: []byte("<!doctype html><title>Embedded browser fixture</title>")},
		"third-party-licenses.txt":     {Data: []byte("Bundled dependency licenses\n")},
		"assets/index-Ab12_c-D.js":     {Data: []byte("export const fixture = true;\n")},
		"assets/index-0123abcd.css":    {Data: []byte("body { color: red; }\n")},
		"assets/large-Ab12_c-D.js":     {Data: []byte(strings.Repeat("export const fixture = true;\n", 200))},
		"assets/large-0123abcd.css":    {Data: []byte(strings.Repeat("body { color: red; }\n", 200))},
		"assets/large.js":              {Data: []byte(strings.Repeat("export const fixture = true;\n", 200))},
		"assets/large-Ab12_c-D.svg":    {Data: []byte(strings.Repeat("<svg/>", 200))},
		"assets/large-Ab12_c-D.woff2":  {Data: []byte(strings.Repeat("synthetic font", 200))},
		"assets/logo.svg":              {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
		"assets/font-Abc12345.woff2":   {Data: []byte("synthetic font")},
		"assets/index-Ab12_c-D.js.map": {Data: []byte("private source-map fixture")},
		"assets/manifest.json":         {Data: []byte("private manifest fixture")},
		"assets/package.json":          {Data: []byte("private package fixture")},
		"assets/.hidden.js":            {Data: []byte("private hidden fixture")},
		"assets/nested/index.js":       {Data: []byte("export const nested = true;")},
		"package.json":                 {Data: []byte("private root package fixture")},
	})
}

func request(t *testing.T, handler http.Handler, method, path string, acceptEncoding ...string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, nil)
	for _, field := range acceptEncoding {
		r.Header.Add("Accept-Encoding", field)
	}
	handler.ServeHTTP(w, r)
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("missing browser security headers")
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "connect-src 'self'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("missing CSP directive %q", directive)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "https:") || strings.Contains(csp, "*") {
		t.Fatal("CSP permits an unrequested script or external origin")
	}
	return w
}

func TestKnownBrowserRoutesServeOnlyEmbeddedIndex(t *testing.T) {
	handler := browserFixture()
	for _, path := range []string{"/", "/?ignored=query", "/projects/project_1", "/projects/" + strings.Repeat("a", 128), "/projects/p/jobs", "/projects/p/jobs?status=open", "/projects/p/jobs/job-1", "/projects/p/inbox", "/projects/p/inbox/conv_1", "/projects/p/workers", "/projects/p/settings", "/projects/p/reviews", "/projects/p/reviews/pulls/1", "/projects/p/reviews/pulls/2147483647?snapshot=review_snapshot_1", "/projects/p/reviews/runs/review_run_1", "/projects/p/workers/w", "/projects/p/notes", "/projects/p/notes?worker_id=w&status=pending", "/projects/p/notes/note_1", "/projects/p/notes/note_1/history/1", "/projects/p/notes/note_1/history/9223372036854775807", "/projects/p/map", "/projects/p/map/entries/map_entry_1", "/projects/p/map/entries/map_entry_1/history/1", "/projects/p/map/entries/map_entry_1/history/9223372036854775807"} {
		t.Run(path, func(t *testing.T) {
			w := request(t, handler, http.MethodGet, path)
			if w.Code != 200 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "Embedded browser fixture") {
				t.Fatalf("known UI route did not serve index: %d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Fatal("HTML content length differs")
			}
		})
	}
}

func TestReviewHistoryDirectNavigationAndReload(t *testing.T) {
	handler := browserFixture()
	for _, path := range []string{"/projects/project_1/reviews/history", "/projects/project_1/reviews/history?cursor=opaque"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := request(t, handler, method, path)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("history deep link failed for %s: %d", method, w.Code)
			}
			if method == http.MethodGet && !strings.Contains(w.Body.String(), "Embedded browser fixture") || method == http.MethodHead && w.Body.Len() != 0 {
				t.Fatal("history navigation did not preserve embedded GET/HEAD behavior")
			}
		}
	}
	for _, path := range []string{"/projects/project_1/reviews/history/extra", "/projects/project_1/reviews/historical", "/projects/project_1/reviews/%68istory", "/projects/project_1/reviews/history%2Fextra"} {
		assertJSONError(t, request(t, handler, http.MethodGet, path), http.StatusNotFound, "not_found")
	}
	assertJSONError(t, request(t, handler, http.MethodPost, "/projects/project_1/reviews/history"), http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestSharedProjectDirectNavigationAndReload(t *testing.T) {
	handler := browserFixture()
	for _, path := range []string{
		"/projects/firemud/project", "/projects/firemud/project?signin=1",
		"/projects/project_one/published/history/1", "/projects/legacy-public/published/history/1?generation=public_generation_fixture",
		"/projects/firemud/published/history/9223372036854775807",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := request(t, handler, method, path)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("shared project direct %s %s: %d", method, path, w.Code)
			}
			if method == http.MethodGet && !strings.Contains(w.Body.String(), "Embedded browser fixture") || method == http.MethodHead && w.Body.Len() != 0 {
				t.Fatal("shared project direct GET/HEAD did not preserve embedded response")
			}
		}
	}
	for _, path := range []string{
		"/projects/firemud/project/extra", "/projects/firemud/%70roject", "/projects/firemud/project%2Fextra",
		"/projects/firemud/published", "/projects/firemud/published/history", "/projects/firemud/published/history/0",
		"/projects/firemud/published/history/-1", "/projects/firemud/published/history/01", "/projects/firemud/published/history/1a",
		"/projects/firemud/published/history/9223372036854775808", "/projects/firemud/published/history/1/extra",
		"/projects/firemud/published/%68istory/1", "/projects/firemud/published/history/1%2Fextra",
	} {
		assertJSONError(t, request(t, handler, http.MethodGet, path), http.StatusNotFound, "not_found")
	}
	for _, path := range []string{"/projects/firemud/project", "/projects/firemud/published/history/1"} {
		assertJSONError(t, request(t, handler, http.MethodPost, path), http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func assertJSONError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var got struct {
		Error struct {
			Code, Message string
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if w.Code != status || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error.Code != code || got.Error.Message == "" || got.RequestID == "" || got.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("expected safe JSON %s, received %d %s", code, w.Code, w.Body.String())
	}
	for _, marker := range []string{"Embedded browser fixture", "private ", "/home/", "index.html", "open "} {
		if strings.Contains(w.Body.String(), marker) {
			t.Fatalf("error disclosed an internal fixture/path marker %q", marker)
		}
	}
}

func TestUnknownRoutesAssetsAndTraversalStayJSONNotFound(t *testing.T) {
	handler := browserFixture()
	for _, path := range []string{
		"/api", "/api/v1/missing", "/api/v1/projects/p", "/healthz", "/healthz/missing", "/readyz/missing",
		"/unknown", "/index.html", "/package.json", "/projects", "/projects/", "/projects/p/", "/projects/p/unknown", "/projects/p/settings/extra", "/projects/p/workers/w/extra", "/projects/p/inbox/c/extra",
		"/projects/p/reviews/pulls", "/projects/p/reviews/runs", "/projects/p/reviews/unknown/1", "/projects/p/reviews/pulls/01", "/projects/p/reviews/pulls/0", "/projects/p/reviews/pulls/-1", "/projects/p/reviews/pulls/2147483648", "/projects/p/reviews/pulls/1a", "/projects/p/reviews/pulls/1/extra", "/projects/p/reviews/runs/run.json", "/projects/p/reviews/runs/" + strings.Repeat("a", 129),
		"/projects/" + strings.Repeat("a", 129), "/projects/p/jobs/" + strings.Repeat("b", 129), "/projects/p.json", "/projects/雪", "/projects/a%20b",
		"/projects/p/notes/note_1/extra", "/projects/p/notes/note_1/history", "/projects/p/notes/note_1/history/0", "/projects/p/notes/note_1/history/01", "/projects/p/notes/note_1/history/-1", "/projects/p/notes/note_1/history/9223372036854775808", "/projects/p/notes/note_1/history/1/extra", "/projects/p/notes/" + strings.Repeat("n", 129), "/projects/p/map/entries", "/projects/p/map/unknown/map_entry_1", "/projects/p/map/entries/map_entry_1/extra", "/projects/p/map/entries/map_entry_1/history", "/projects/p/map/entries/map_entry_1/history/0", "/projects/p/map/entries/map_entry_1/history/01", "/projects/p/map/entries/map_entry_1/history/-1", "/projects/p/map/entries/map_entry_1/history/9223372036854775808", "/projects/p/map/entries/map_entry_1/history/1/extra", "/projects/p/map/entries/" + strings.Repeat("a", 129),
		"/assets", "/assets/", "/assets/missing.js", "/assets/index-Ab12_c-D.js.map", "/assets/manifest.json", "/assets/package.json", "/assets/.hidden.js", "/assets/nested", "/assets/INDEX.JS",
		"//projects/p", "/projects//p", "/projects/../p", "/projects/./p", "/assets/../index.html", "/assets/nested/../../index.html", "/projects/p%2Fjobs%2Fj", "/projects/p%2fjobs%2fj", "/projects/%2e%2e", "/projects/%252e%252e", "/projects/p%5Cj", "/assets/%69ndex-Ab12_c-D.js", "/assets/nested%2Findex.js",
	} {
		t.Run(path, func(t *testing.T) {
			assertJSONError(t, request(t, handler, http.MethodGet, path), http.StatusNotFound, "not_found")
		})
	}
	// Reserved API and health paths never become HTML or a static-method result.
	for _, path := range []string{"/api/missing", "/healthz/missing", "/readyz/missing"} {
		assertJSONError(t, request(t, handler, http.MethodPost, path), http.StatusNotFound, "not_found")
	}
}

func TestAssetsHaveExplicitMIMEAndOnlyFingerprintedNamesAreImmutable(t *testing.T) {
	handler := browserFixture()
	for _, tc := range []struct {
		path, mime string
		immutable  bool
	}{
		{"/third-party-licenses.txt", "text/plain; charset=utf-8", false},
		{"/assets/index-Ab12_c-D.js", "text/javascript; charset=utf-8", true},
		{"/assets/index-0123abcd.css", "text/css; charset=utf-8", true},
		{"/assets/logo.svg", "image/svg+xml", false},
		{"/assets/font-Abc12345.woff2", "font/woff2", true},
		{"/assets/nested/index.js", "text/javascript; charset=utf-8", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := request(t, handler, http.MethodGet, tc.path)
			if w.Code != 200 || w.Header().Get("Content-Type") != tc.mime || w.Body.Len() == 0 || w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Fatal("asset content, MIME or length differs")
			}
			cache := w.Header().Get("Cache-Control")
			if tc.immutable && cache != "public, max-age=31536000, immutable" || !tc.immutable && cache != "no-store" {
				t.Fatalf("unexpected asset cache policy %q", cache)
			}
			head := request(t, handler, http.MethodHead, tc.path)
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != w.Header().Get("Content-Length") || head.Header().Get("Content-Type") != tc.mime || head.Header().Get("Cache-Control") != cache {
				t.Fatal("HEAD did not preserve asset headers without body")
			}
		})
	}
}

func TestImmutableBundleEncodingNegotiationAndHEAD(t *testing.T) {
	handler := browserFixture()
	for _, path := range []string{"/assets/large-Ab12_c-D.js", "/assets/large-0123abcd.css"} {
		identity := request(t, handler, http.MethodGet, path)
		for _, tc := range []struct {
			name   string
			fields []string
			gzip   bool
		}{
			{"absent", nil, false},
			{"empty", []string{""}, false},
			{"gzip", []string{"gzip"}, true},
			{"case", []string{" GZip ; Q=1.000 "}, true},
			{"identity default preference", []string{"br, gzip;q=0.001"}, false},
			{"positive with identity excluded", []string{"identity;q=0, gzip;q=0.001"}, true},
			{"identity", []string{"identity"}, false},
			{"br only", []string{"br"}, false},
			{"excluded", []string{"gzip;q=0"}, false},
			{"wildcard", []string{"*"}, true},
			{"wildcard lower preference", []string{"*;q=0.5"}, false},
			{"wildcard excluded", []string{"*;q=0"}, false},
			{"explicit exclusion", []string{"gzip;q=0, *;q=1"}, false},
			{"explicit positive", []string{"*;q=0, gzip;q=0.5"}, true},
			{"identity preferred", []string{"gzip;q=0.5, identity;q=0.9"}, false},
			{"gzip preferred", []string{"gzip;q=0.9, identity;q=0.5"}, true},
			{"equal preference", []string{"gzip;q=0.5, identity;q=0.5"}, true},
			{"multiple fields", []string{"br", "gzip;q=0.8", "identity;q=0.7"}, true},
			{"multiple exclusion", []string{"*", "gzip;q=0"}, false},
			{"multiple identity preference", []string{"gzip;q=0.5", "identity"}, false},
			{"duplicate positive", []string{"gzip;q=0.5", "GZIP;q=0.8", "identity;q=0.7"}, true},
			{"duplicate exclusion first", []string{"gzip;q=0", "gzip"}, false},
			{"duplicate exclusion last", []string{"gzip", "gzip;q=0"}, false},
			{"missing quality", []string{"gzip;q="}, false},
			{"invalid quality", []string{"gzip;q=invalid, *"}, false},
			{"nan quality", []string{"gzip;q=NaN"}, false},
			{"negative quality", []string{"gzip;q=-0.1"}, false},
			{"too high quality", []string{"gzip;q=1.001"}, false},
			{"integer too high", []string{"gzip;q=2"}, false},
			{"exponent quality", []string{"gzip;q=1e0"}, false},
			{"excess precision", []string{"gzip;q=0.0001"}, false},
			{"missing leading zero", []string{"gzip;q=.5"}, false},
			{"quoted quality", []string{`gzip;q="1"`}, false},
			{"unknown parameter", []string{"gzip;level=1"}, false},
			{"duplicate parameter", []string{"gzip;q=1;q=0"}, false},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				get := request(t, handler, http.MethodGet, path, tc.fields...)
				if get.Code != http.StatusOK || get.Header().Get("Vary") != "Accept-Encoding" || get.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || get.Header().Get("Content-Type") != identity.Header().Get("Content-Type") || get.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
					t.Fatalf("representation metadata differs: %d %v", get.Code, get.Header())
				}
				if tc.gzip {
					assertGzipBytes(t, get, identity.Body.Bytes())
				} else if get.Header().Get("Content-Encoding") != "" || !bytes.Equal(get.Body.Bytes(), identity.Body.Bytes()) || get.Header().Get("Content-Length") != identity.Header().Get("Content-Length") {
					t.Fatal("identity representation changed")
				}
				head := request(t, handler, http.MethodHead, path, tc.fields...)
				if head.Code != get.Code || head.Body.Len() != 0 {
					t.Fatal("HEAD status or body differs")
				}
				for _, key := range []string{"Content-Encoding", "Content-Length", "Content-Type", "Cache-Control", "Vary"} {
					if head.Header().Get(key) != get.Header().Get(key) {
						t.Fatalf("HEAD %s differs from GET", key)
					}
				}
			})
		}
	}
}

func assertGzipBytes(t *testing.T, response *httptest.ResponseRecorder, original []byte) {
	t.Helper()
	if response.Header().Get("Content-Encoding") != "gzip" || response.Body.Len() >= len(original) {
		t.Fatal("expected a smaller gzip representation")
	}
	reader, err := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(decoded, original) {
		t.Fatalf("gzip did not preserve original asset bytes: read=%v close=%v", err, closeErr)
	}
}

func TestGzipLeavesOtherResourcesAndErrorsUnchanged(t *testing.T) {
	handler := browserFixture()
	for _, path := range []string{
		"/", "/projects/p/jobs", "/public/project", "/browser-build.json", "/third-party-licenses.txt",
		"/assets/index-Ab12_c-D.js", "/assets/index-0123abcd.css", // gzip would be larger
		"/assets/large.js", "/assets/large-Ab12_c-D.svg", "/assets/large-Ab12_c-D.woff2",
		"/assets/nested/index.js", "/assets/site.webmanifest", "/assets/missing-Ab12_c-D.js",
		"/api/v1/projects/p", "/assets/index-Ab12_c-D.js.map", "/assets/%69ndex-Ab12_c-D.js",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			plain := request(t, handler, method, path)
			encoded := request(t, handler, method, path, "gzip, *")
			if encoded.Code != plain.Code || encoded.Header().Get("Content-Encoding") != "" || encoded.Header().Get("Vary") != "" {
				t.Fatalf("gzip changed ineligible response: %s %s", method, path)
			}
			for _, key := range []string{"Content-Length", "Content-Type", "Cache-Control"} {
				if encoded.Header().Get(key) != plain.Header().Get(key) {
					t.Fatalf("gzip changed %s for %s %s", key, method, path)
				}
			}
			// Error bodies contain independent request IDs; successful bytes must match.
			if plain.Code == http.StatusOK && !bytes.Equal(encoded.Body.Bytes(), plain.Body.Bytes()) {
				t.Fatalf("gzip changed ineligible bytes: %s %s", method, path)
			}
			if method == http.MethodHead && encoded.Body.Len() != 0 {
				t.Fatal("ineligible HEAD wrote a body")
			}
		}
	}
}

func TestBrowserMethodsAndMissingBuildAreSafe(t *testing.T) {
	handler := browserFixture()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
		w := request(t, handler, method, "/")
		assertJSONError(t, w, http.StatusMethodNotAllowed, "method_not_allowed")
		if w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("method error omitted Allow")
		}
	}
	w := request(t, handler, http.MethodHead, "/projects/p")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("HEAD SPA route did not preserve HTML metadata")
	}
	w = request(t, handler, http.MethodHead, "/assets/missing.js")
	if w.Code != 404 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Request-ID") == "" {
		t.Fatal("HEAD error wrote content or omitted safe metadata")
	}
	assertJSONError(t, request(t, handlerFS(fstest.MapFS{}), http.MethodGet, "/projects/p"), http.StatusServiceUnavailable, "browser_unavailable")
}

func TestPublicHandlerUsesEmbeddedBuild(t *testing.T) {
	// The embedded HTML may be regenerated by the owning UI build lane. This
	// checks the public constructor without coupling to placeholder/UI wording.
	handler := Handler()
	w := request(t, handler, http.MethodGet, "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || w.Body.Len() == 0 {
		t.Fatal("public handler did not serve embedded browser build")
	}
	entries, err := fs.ReadDir(assets, "assets/assets")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		name := "assets/" + entry.Name()
		if !bundledHash.MatchString(name) || (!strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".css")) {
			continue
		}
		original, err := fs.ReadFile(assets, "assets/"+name)
		if err != nil {
			t.Fatal(err)
		}
		plain := request(t, handler, http.MethodGet, "/"+name)
		encoded := request(t, handler, http.MethodGet, "/"+name, "gzip")
		if plain.Code != http.StatusOK || encoded.Code != http.StatusOK || !bytes.Equal(plain.Body.Bytes(), original) || plain.Header().Get("Content-Length") != strconv.Itoa(len(original)) {
			t.Fatal("embedded identity asset changed")
		}
		assertGzipBytes(t, encoded, original)
		count++
	}
	if count < 2 {
		t.Fatal("embedded JS/CSS proof did not exercise both bundles")
	}
}

func TestPublicProjectRoutesRemainExactAndServeNoProjectData(t *testing.T) {
	handler := browserFixture()
	for _, path := range []string{"/public/a", "/public/project-1", "/public/" + strings.Repeat("a", 64), "/public/project-1/history/1?generation=public_generation_fixture", "/public/project-1/history/9223372036854775807"} {
		w := request(t, handler, http.MethodGet, path)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "Embedded browser fixture") {
			t.Fatalf("public SPA route unavailable: %s %d", path, w.Code)
		}
	}
	for _, path := range []string{"/public", "/public/", "/public/UPPER", "/public/-bad", "/public/bad-", "/public/under_score", "/public/" + strings.Repeat("a", 65), "/public/a/private", "/public/a/history", "/public/a/history/0", "/public/a/history/01", "/public/a/history/-1", "/public/a/history/9223372036854775808", "/public/a/history/1/extra", "/public/a%2fhistory%2f1"} {
		assertJSONError(t, request(t, handler, http.MethodGet, path), 404, "not_found")
	}
}
