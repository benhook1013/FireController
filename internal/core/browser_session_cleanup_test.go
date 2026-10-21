package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionCleanupPreservesNewestAndBoundsEncodedBytes(t *testing.T) {
	s, _, _ := streamFixture(t)
	for _, origin := range []string{"http://localhost", "https://controller.test"} {
		t.Run(origin, func(t *testing.T) {
			r := httptest.NewRequest("POST", origin+"/api/v1/projects/project/browser-session", nil)
			r.SetPathValue("project_id", "project")
			r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
			r.Header.Set("X-FC-Instance", s.InstanceID())
			_, current, err := s.CreateBrowserSession(r, strings.Repeat("f", 32), false)
			if err != nil {
				t.Fatal(err)
			}
			var original []*http.Cookie
			for i := 0; i < 24; i++ {
				id := fmt.Sprintf("%032x", i+1)
				cookie := browserSessionCookie(r, id)
				claims := browserSessionClaims{SessionID: id, InstanceID: s.InstanceID(), ProjectID: strings.Repeat("p", 128), CredentialHash: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(browserSessionLifetime - time.Duration(i+1)*time.Minute).Unix()}
				cookie.Value, err = s.browserSessionCodec().Encode(cookie.Name, claims)
				if err != nil {
					t.Fatal(err)
				}
				r.AddCookie(cookie)
				original = append(original, cookie)
			}
			// Cookie scopes omit ports. A different instance on this host and
			// unrelated applications must not lose their cookies during cleanup.
			foreign := browserSessionCookie(r, strings.Repeat("e", 32))
			foreign.Value = "encrypted-by-a-different-instance"
			r.AddCookie(foreign)
			r.AddCookie(&http.Cookie{Name: "another-application", Value: "unchanged"})
			removed := map[string]bool{}
			for _, cookie := range s.PruneBrowserSessions(r, current) {
				if cookie.Name == current.Name || cookie.Name == foreign.Name || cookie.Name == "another-application" || cookie.MaxAge != -1 || !cookie.Expires.Before(time.Now()) || cookie.Path != "/" || cookie.Secure != current.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
					t.Fatal("cleanup crossed identity or weakened cookie flags", cookie)
				}
				removed[cookie.Name] = true
			}
			count, bytes := 1, len(current.Name)+len(current.Value)+1
			for _, cookie := range original {
				if !removed[cookie.Name] {
					count++
					bytes += len(cookie.Name) + len(cookie.Value) + 3
				}
			}
			if count > browserSessionCookieLimit || count < 2 || bytes > browserSessionCookieBudget || removed[original[0].Name] || !removed[original[len(original)-1].Name] {
				t.Fatal("cookie retention is unbounded or discarded newest session", count, bytes, removed)
			}
		})
	}
}

func TestBrowserSessionHTTPSCleanupIncludesLocalHTTPCookies(t *testing.T) {
	s, _, _ := streamFixture(t)
	request := func(origin string) *http.Request {
		r := httptest.NewRequest("POST", origin+"/api/v1/projects/project/browser-session", nil)
		r.SetPathValue("project_id", "project")
		r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
		r.Header.Set("X-FC-Instance", s.InstanceID())
		return r
	}
	secure := request("https://localhost")
	_, current, err := s.CreateBrowserSession(secure, strings.Repeat("f", 32), false)
	if err != nil {
		t.Fatal(err)
	}
	var prior []*http.Cookie
	for i := 1; i <= 24; i++ {
		origin := "https://localhost"
		if i%2 == 0 {
			origin = "http://localhost"
		}
		_, cookie, err := s.CreateBrowserSession(request(origin), fmt.Sprintf("%032x", i), false)
		if err != nil {
			t.Fatal(err)
		}
		secure.AddCookie(cookie)
		prior = append(prior, cookie)
	}
	removed := map[string]bool{}
	removedLocal := false
	for _, cookie := range s.PruneBrowserSessions(secure, current) {
		if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.MaxAge != -1 {
			t.Fatal("HTTPS cleanup weakened expiration flags", cookie)
		}
		removed[cookie.Name] = true
		removedLocal = removedLocal || strings.HasPrefix(cookie.Name, "fc-local-browser-")
	}
	count, bytes := 1, len(current.Name)+len(current.Value)+1
	for _, cookie := range prior {
		if !removed[cookie.Name] {
			count++
			bytes += len(cookie.Name) + len(cookie.Value) + 3
		}
	}
	if !removedLocal || count > browserSessionCookieLimit || bytes > browserSessionCookieBudget {
		t.Fatal("HTTPS ignored cookies retained from HTTP", count, bytes, removed)
	}
}
