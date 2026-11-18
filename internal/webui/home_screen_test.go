package webui

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

func TestHomeScreenAssets(t *testing.T) {
	h := Handler()
	for _, route := range []string{"/", "/projects/firemud/reviews", "/projects/firecontroller"} {
		w := request(t, h, http.MethodGet, route)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `rel="apple-touch-icon"`) || !strings.Contains(w.Body.String(), `href="/assets/site.webmanifest"`) {
			t.Fatalf("home screen metadata missing on %s", route)
		}
	}
	w := request(t, h, http.MethodGet, "/assets/site.webmanifest")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("manifest response: %d %v", w.Code, w.Header())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"start_url", "id", "scope", "display", "display_override"} {
		if _, ok := m[field]; ok {
			t.Fatalf("%s must not override saved shortcut behavior", field)
		}
	}
	var icons []struct{ Src, Sizes, Type string }
	if err := json.Unmarshal(m["icons"], &icons); err != nil {
		t.Fatal(err)
	}
	if len(icons) != 2 {
		t.Fatalf("unexpected icon count %d", len(icons))
	}
	expected := map[string]string{"/assets/icon-192.png": "192x192", "/assets/icon-512.png": "512x512"}
	for _, icon := range icons {
		if expected[icon.Src] == "" || expected[icon.Src] != icon.Sizes || icon.Type != "image/png" {
			t.Fatalf("invalid icon %+v", icon)
		}
		delete(expected, icon.Src)
	}
	if len(expected) != 0 {
		t.Fatal("missing manifest icons")
	}
	for path, size := range map[string]int{"/assets/apple-touch-icon.png": 180, "/assets/icon-192.png": 192, "/assets/icon-512.png": 512} {
		get := request(t, h, http.MethodGet, path)
		if get.Code != 200 || get.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("icon %s: %d", path, get.Code)
		}
		img, err := png.Decode(bytes.NewReader(get.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Fatalf("incorrect dimensions for %s", path)
		}
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				_, _, _, a := img.At(x, y).RGBA()
				if a != 65535 {
					t.Fatalf("non-opaque icon %s", path)
				}
			}
		}
		head := request(t, h, http.MethodHead, path)
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
			t.Fatalf("invalid HEAD for %s", path)
		}
	}
	head := request(t, h, http.MethodHead, "/assets/site.webmanifest")
	if head.Code != 200 || head.Body.Len() != 0 {
		t.Fatal("manifest HEAD")
	}
	assertJSONError(t, request(t, h, http.MethodPost, "/assets/site.webmanifest"), 405, "method_not_allowed")
	for _, path := range []string{"/assets/other.webmanifest", "/site.webmanifest", "/assets/package.json", "/assets/%73ite.webmanifest", "/api/site.webmanifest"} {
		assertJSONError(t, request(t, h, http.MethodGet, path), 404, "not_found")
	}
}
