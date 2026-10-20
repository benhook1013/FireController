// Package webui serves only the embedded browser build and its known UI routes.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsitemodel"
)

//go:embed assets
var assets embed.FS

const contentSecurityPolicy = "default-src 'self'; connect-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

var bundledHash = regexp.MustCompile(`-[A-Za-z0-9_-]{8}\.[a-z0-9]+$`)

// Handler serves the compiled browser without access to the host filesystem.
func Handler() http.Handler {
	files, err := fs.Sub(assets, "assets")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			securityHeaders(w)
			writeError(w, r, "browser_unavailable", "The browser build is unavailable.", http.StatusServiceUnavailable)
		})
	}
	return handlerFS(files)
}

func handlerFS(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		path := r.URL.Path
		if !safePath(r) || reservedPath(path) {
			writeError(w, r, "not_found", "This browser resource is unavailable.", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, r, "method_not_allowed", "Use GET or HEAD for browser resources.", http.StatusMethodNotAllowed)
			return
		}
		name, contentType, cache := "", "", "no-store"
		if uiRoute(path) {
			name, contentType = "index.html", "text/html; charset=utf-8"
		} else if path == "/third-party-licenses.txt" {
			name, contentType = "third-party-licenses.txt", "text/plain; charset=utf-8"
		} else if strings.HasPrefix(path, "/assets/") {
			name = strings.TrimPrefix(path, "/")
			contentType = assetType(name)
			if contentType != "" && bundledHash.MatchString(name) {
				cache = "public, max-age=31536000, immutable"
			}
		}
		if name == "" || contentType == "" {
			writeError(w, r, "not_found", "This browser resource is unavailable.", http.StatusNotFound)
			return
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			if name == "index.html" {
				writeError(w, r, "browser_unavailable", "The browser build is unavailable.", http.StatusServiceUnavailable)
			} else {
				writeError(w, r, "not_found", "This browser resource is unavailable.", http.StatusNotFound)
			}
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", cache)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	})
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// Reject ambiguous escaping and path normalization instead of redirecting a
// traversal or an encoded separator into a permitted SPA route.
func safePath(r *http.Request) bool {
	path := r.URL.Path
	if path == "" || len(path) > 2048 || path[0] != '/' || r.URL.RawPath != "" || strings.ContainsAny(path, "%\\") {
		return false
	}
	if path == "/" {
		return true
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || segment[0] == '.' {
			return false
		}
		for _, ch := range segment {
			if !asciiID(ch) && ch != '.' {
				return false
			}
		}
	}
	return true
}

func reservedPath(path string) bool {
	for _, prefix := range []string{"/api", "/healthz", "/readyz"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func uiRoute(path string) bool {
	if path == "/" {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "public" && publicsitemodel.ValidSlug(parts[1]) {
		if len(parts) == 2 {
			return true
		}
		if len(parts) == 4 && parts[2] == "history" {
			revision, err := strconv.ParseInt(parts[3], 10, 64)
			return err == nil && revision > 0 && strconv.FormatInt(revision, 10) == parts[3]
		}
		return false
	}
	if len(parts) < 2 || parts[0] != "projects" || !validID(parts[1]) {
		return false
	}
	if len(parts) == 2 {
		return true
	}
	if len(parts) == 3 {
		return parts[2] == "project" || parts[2] == "jobs" || parts[2] == "inbox" || parts[2] == "workers" || parts[2] == "reviews" || parts[2] == "map" || parts[2] == "notes" || parts[2] == "settings"
	}
	if len(parts) == 5 && parts[2] == "published" && parts[3] == "history" {
		revision, err := strconv.ParseInt(parts[4], 10, 64)
		return err == nil && revision > 0 && strconv.FormatInt(revision, 10) == parts[4]
	}
	if len(parts) == 4 && parts[2] == "reviews" && parts[3] == "history" {
		return true
	}
	if len(parts) == 6 && parts[2] == "notes" && validID(parts[3]) && parts[4] == "history" {
		revision, err := strconv.ParseInt(parts[5], 10, 64)
		return err == nil && revision > 0 && strconv.FormatInt(revision, 10) == parts[5]
	}
	if parts[2] == "map" && len(parts) >= 5 && parts[3] == "entries" && validID(parts[4]) {
		if len(parts) == 5 {
			return true
		}
		if len(parts) == 7 && parts[5] == "history" {
			revision, err := strconv.ParseInt(parts[6], 10, 64)
			return err == nil && revision > 0 && strconv.FormatInt(revision, 10) == parts[6]
		}
	}
	if len(parts) == 5 && parts[2] == "reviews" {
		if parts[3] == "runs" {
			return validID(parts[4])
		}
		if parts[3] == "pulls" {
			number, err := strconv.ParseInt(parts[4], 10, 32)
			return err == nil && number > 0 && strconv.FormatInt(number, 10) == parts[4]
		}
	}
	return len(parts) == 4 && (parts[2] == "jobs" || parts[2] == "inbox" || parts[2] == "workers" || parts[2] == "notes") && validID(parts[3])
}

func validID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, ch := range value {
		if !asciiID(ch) {
			return false
		}
	}
	return true
}

func asciiID(ch rune) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_'
}

func assetType(name string) string {
	position := strings.LastIndexByte(name, '.')
	if position == -1 {
		return ""
	}
	switch name[position:] {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".ico":
		return "image/x-icon"
	}
	return ""
}

type headWriter struct{ http.ResponseWriter }

func (headWriter) Write(data []byte) (int, error) { return len(data), nil }

func writeError(w http.ResponseWriter, r *http.Request, code, message string, status int) {
	if r.Method == http.MethodHead {
		w = headWriter{w}
	}
	core.WriteError(w, core.Fail(code, message, status))
}
