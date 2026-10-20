package webui

import (
	"encoding/json"
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

func request(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
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
	w := request(t, Handler(), http.MethodGet, "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || w.Body.Len() == 0 {
		t.Fatal("public handler did not serve embedded browser build")
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
