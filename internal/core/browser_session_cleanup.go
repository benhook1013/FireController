package core

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

const browserSessionCookieLimit = 8
const browserSessionCookieBudget = 6 << 10

// PruneBrowserSessions bounds this instance's retained browser cookies when an
// authenticated connection succeeds. Other instances on the same host may use
// the same cookie prefix, so undecodable cookies are deliberately untouched.
// The new connection always survives; older tabs may need to connect again.
func (s *Store) PruneBrowserSessions(r *http.Request, current *http.Cookie) []*http.Cookie {
	if current == nil {
		return nil
	}
	type retained struct {
		cookie  *http.Cookie
		expires int64
	}
	var candidates []retained
	seen := map[string]bool{}
	codec := s.browserSessionCodec()
	now := time.Now()
	for _, cookie := range r.Cookies() {
		if cookie.Name == current.Name || seen[cookie.Name] {
			continue
		}
		seen[cookie.Name] = true
		prefix := ""
		switch {
		case strings.HasPrefix(cookie.Name, "__Host-fc-browser-") && !BrowserSessionLocalHTTP(r):
			prefix = "__Host-fc-browser-"
		case strings.HasPrefix(cookie.Name, "fc-local-browser-"):
			prefix = "fc-local-browser-"
		default:
			continue
		}
		id := strings.TrimPrefix(cookie.Name, prefix)
		if !browserSessionID.MatchString(id) {
			continue
		}
		var claims browserSessionClaims
		if codec.Decode(cookie.Name, cookie.Value, &claims) != nil || claims.SessionID != id || claims.InstanceID != s.instanceID || !browserSessionIdentity.MatchString(claims.ProjectID) || !browserCredentialHash.MatchString(claims.CredentialHash) || claims.ExpiresAt <= now.Unix() || claims.ExpiresAt > now.Add(browserSessionLifetime+5*time.Second).Unix() {
			continue
		}
		candidates = append(candidates, retained{cookie: cookie, expires: claims.ExpiresAt})
	}
	slices.SortFunc(candidates, func(a, b retained) int {
		if a.expires > b.expires {
			return -1
		}
		if a.expires < b.expires {
			return 1
		}
		return strings.Compare(a.cookie.Name, b.cookie.Name)
	})
	count, bytes := 1, len(current.Name)+len(current.Value)+1
	var expired []*http.Cookie
	for _, candidate := range candidates {
		size := len(candidate.cookie.Name) + len(candidate.cookie.Value) + 3
		if count < browserSessionCookieLimit && bytes+size <= browserSessionCookieBudget {
			count++
			bytes += size
			continue
		}
		cookie := browserSessionCookie(r, "")
		cookie.Name = candidate.cookie.Name
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0).UTC()
		expired = append(expired, cookie)
	}
	return expired
}
