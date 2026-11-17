package webui

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestBackupInformationPagesArePublicAndIndependent(t *testing.T) {
	handler := BackupInfoHandler()
	for _, path := range []string{"/about", "/privacy", "/backup-info.css"} {
		get := request(t, handler, http.MethodGet, path)
		if get.Code != http.StatusOK || get.Header().Get("Cache-Control") != "no-store" || get.Header().Get("Set-Cookie") != "" {
			t.Fatalf("public information needs a session or was not served: %s: %d", path, get.Code)
		}
		if get.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
			t.Fatalf("wrong content length: %s", path)
		}
		head := request(t, handler, http.MethodHead, path)
		if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
			t.Fatalf("HEAD does not match GET: %s", path)
		}
		post := request(t, handler, http.MethodPost, path)
		assertJSONError(t, post, http.StatusMethodNotAllowed, "method_not_allowed")
		if post.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("missing supported methods: %s", path)
		}
	}
	for _, path := range []string{"/about", "/privacy"} {
		page := request(t, handler, http.MethodGet, path+"?credential=untrusted-query-marker").Body.String()
		if strings.Contains(page, "untrusted-query-marker") || strings.Contains(page, "<script") || !strings.Contains(page, `href="/privacy"`) || !strings.Contains(page, `href="/about"`) {
			t.Fatalf("public information reflected input, loaded script or lost policy navigation: %s", path)
		}
	}
	privacy := request(t, handler, http.MethodGet, "/privacy").Body.String()
	for _, disclosure := range []string{"drive.file", "refresh tokens", "Optional rclone crypt", "Disconnecting does not erase existing backups", "Google API Services User Data Policy"} {
		if !strings.Contains(privacy, disclosure) {
			t.Fatalf("missing data-handling disclosure: %s", disclosure)
		}
	}
}

func TestBackupInformationRoutesDoNotWidenBrowserAccess(t *testing.T) {
	handler := BackupInfoHandler()
	for _, path := range []string{"/", "/projects/private", "/api/v1/projects/private", "/about/extra", "/privacy/extra", "/%61bout", "/privacy%2Fextra", "/backup-info.css/extra"} {
		assertJSONError(t, request(t, handler, http.MethodGet, path), http.StatusNotFound, "not_found")
	}
}
