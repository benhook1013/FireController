package webui

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

func buildFixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                {Data: []byte(`<script type="module" crossorigin src="/assets/index-Ab12_c-D.js"></script><link rel="stylesheet" href="/assets/index-0123abcd.css">`)},
		"assets/index-Ab12_c-D.js":  {Data: []byte(`console.log("public");`)},
		"assets/index-0123abcd.css": {Data: []byte(`body{color:red}`)},
	}
}
func TestBrowserBuildPublicIdentityAndMethods(t *testing.T) {
	files := buildFixture()
	get := request(t, handlerFS(files), http.MethodGet, "/browser-build.json")
	if get.Code != http.StatusOK || get.Header().Get("Cache-Control") != "no-store" || get.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("invalid manifest headers: %d %v", get.Code, get.Header())
	}
	var value struct {
		Version int      `json:"version"`
		Assets  []string `json:"assets"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &value); err != nil || value.Version != 1 || len(value.Assets) != 2 || value.Assets[0] != "/assets/index-0123abcd.css" || value.Assets[1] != "/assets/index-Ab12_c-D.js" {
		t.Fatalf("unexpected public manifest: %s", get.Body.String())
	}
	head := request(t, handlerFS(files), http.MethodHead, "/browser-build.json")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Fatal("HEAD differs from GET")
	}
	assertJSONError(t, request(t, handlerFS(files), http.MethodPost, "/browser-build.json"), http.StatusMethodNotAllowed, "method_not_allowed")
	// A backend-only deployment has exactly this same frontend identity.
	again, err := browserBuild(buildFixture())
	if err != nil || string(again) != get.Body.String() {
		t.Fatal("unchanged assets changed build identity")
	}
	files["index.html"].Data = []byte(strings.ReplaceAll(string(files["index.html"].Data), "0123abcd", "9876abcd"))
	files["assets/index-9876abcd.css"] = &fstest.MapFile{Data: []byte(`body{color:blue}`)}
	changed, err := browserBuild(files)
	if err != nil || string(changed) == string(again) {
		t.Fatal("CSS deployment did not change identity")
	}
}
func TestBrowserBuildRejectsIncompleteOrUnsafeRollouts(t *testing.T) {
	cases := []string{
		``, `<script type="module" src="/assets/index-Ab12_c-D.js"></script>`,
		`<script type="module" src="https://external.test/assets/index-Ab12_c-D.js"></script><link rel="stylesheet" href="/assets/index-0123abcd.css">`,
		`<script type="module" src="/assets/index-Ab12_c-D.js"></script><link rel="stylesheet" href="/assets/missing-0123abcd.css">`,
		`<script type="module" src="/assets/index-Ab12_c-D.js" src="/assets/index-Ab12_c-D.js"></script><link rel="stylesheet" href="/assets/index-0123abcd.css">`,
	}
	for _, index := range cases {
		files := buildFixture()
		files["index.html"].Data = []byte(index)
		assertJSONError(t, request(t, handlerFS(files), http.MethodGet, "/browser-build.json"), http.StatusServiceUnavailable, "browser_unavailable")
	}
	for _, missing := range []string{"index.html", "assets/index-Ab12_c-D.js", "assets/index-0123abcd.css"} {
		files := buildFixture()
		delete(files, missing)
		if _, err := browserBuild(files); err == nil {
			t.Fatalf("missing %s accepted", missing)
		}
	}
	files := buildFixture()
	files["assets/index-Ab12_c-D.js"].Data = nil
	if _, err := browserBuild(files); err == nil {
		t.Fatal("empty asset accepted")
	}
	files = buildFixture()
	files["assets/index-Ab12_c-D.js"].Mode = fs.ModeDir
	if _, err := browserBuild(files); err == nil {
		t.Fatal("nonregular asset accepted")
	}
}
func TestEmbeddedBrowserBuildMatchesEntrypoints(t *testing.T) {
	result := request(t, Handler(), http.MethodGet, "/browser-build.json")
	if result.Code != 200 {
		t.Fatalf("embedded build manifest unavailable: %s", result.Body.String())
	}
}
