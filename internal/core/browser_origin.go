package core

import (
	"net/http"
	"net/url"
	"strings"
)

// BrowserSessionOrigin checks the actual serving origin, without trusting
// forwarding headers. Connection/logout allow non-browser clients to omit
// Origin; cookie-authenticated domain mutations require it.
func BrowserSessionOrigin(r *http.Request, required bool) error {
	bad := Fail("forbidden", "Browser session requests must use the same origin.", 403)
	sites := r.Header.Values("Sec-Fetch-Site")
	if len(sites) > 1 || len(sites) == 1 && sites[0] != "same-origin" && sites[0] != "same-site" && sites[0] != "none" {
		return bad
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 && !required {
		return nil
	}
	if len(origins) != 1 {
		return bad
	}
	origin, err := url.Parse(origins[0])
	scheme := "https"
	if BrowserSessionLocalHTTP(r) {
		scheme = "http"
	}
	if err != nil || strings.ContainsAny(origins[0], "?#") || origin.Scheme != scheme || !strings.EqualFold(origin.Host, r.Host) || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.Opaque != "" || origin.ForceQuery {
		return bad
	}
	return nil
}

// browserMutationAllowed is deliberately an allowlist independent of original
// credential authority. Newly registered handlers do not gain cookie access.
func browserMutationAllowed(r *http.Request, project string) bool {
	base := "/api/v1/projects/" + project + "/"
	if r.Method == http.MethodPut {
		return r.URL.Path == base+"presentation"
	}
	if r.Method != http.MethodPost {
		return false
	}
	if r.URL.Path == base+"messages" {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, base), "/")
	return strings.HasPrefix(r.URL.Path, base) && len(parts) == 3 && parts[0] == "messages" && browserSessionIdentity.MatchString(parts[1]) && (parts[2] == "seen" || parts[2] == "ack")
}
