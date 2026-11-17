package webui

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Only entry fingerprints already public in Vite's HTML are disclosed.
// The identity deliberately excludes backend and source revisions.
var entryTags = regexp.MustCompile(`(?i)<(script|link)\b[^>]{0,1000}>`)
var entryAttributes = regexp.MustCompile(`\s([a-zA-Z]+)="([^"]*)"`)
var entryAsset = regexp.MustCompile(`^/assets/[A-Za-z0-9_-]+-[A-Za-z0-9_-]{8}\.(js|css)$`)

func browserBuild(files fs.FS) ([]byte, error) {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil || len(index) > 64<<10 {
		return nil, fs.ErrInvalid
	}
	paths, seen, modules, styles := []string{}, map[string]bool{}, 0, 0
	for _, tag := range entryTags.FindAllStringSubmatch(string(index), -1) {
		attrs := map[string]string{}
		for _, attr := range entryAttributes.FindAllStringSubmatch(tag[0], -1) {
			key := strings.ToLower(attr[1])
			if _, exists := attrs[key]; exists {
				return nil, fs.ErrInvalid
			}
			attrs[key] = attr[2]
		}
		path := ""
		if strings.EqualFold(tag[1], "script") && attrs["type"] == "module" {
			path = attrs["src"]
			modules++
			if !strings.HasSuffix(path, ".js") {
				return nil, fs.ErrInvalid
			}
		} else if strings.EqualFold(tag[1], "link") && attrs["rel"] == "stylesheet" {
			path = attrs["href"]
			styles++
			if !strings.HasSuffix(path, ".css") {
				return nil, fs.ErrInvalid
			}
		} else {
			continue
		}
		if len(path) > 160 || !entryAsset.MatchString(path) || seen[path] {
			return nil, fs.ErrInvalid
		}
		info, err := fs.Stat(files, strings.TrimPrefix(path, "/"))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fs.ErrInvalid
		}
		seen[path] = true
		paths = append(paths, path)
	}
	if modules != 1 || styles < 1 || len(paths) > 8 {
		return nil, fs.ErrInvalid
	}
	sort.Strings(paths)
	return json.Marshal(struct {
		Version int      `json:"version"`
		Assets  []string `json:"assets"`
	}{1, paths})
}

func serveBrowserBuild(w http.ResponseWriter, r *http.Request, files fs.FS) {
	data, err := browserBuild(files)
	if err != nil {
		writeError(w, r, "browser_unavailable", "The browser build is unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
