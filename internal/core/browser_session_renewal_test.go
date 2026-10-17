package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func renewalCoreRequest(s *Store, id string, cookie *http.Cookie) *http.Request {
	r := browserCoreRequest(s, id, cookie)
	r.Method = http.MethodPost
	r.URL.Path += "/browser-session/renew"
	r.Header.Set("Origin", "http://localhost")
	return r
}
func renewalClaims(t *testing.T, s *Store, cookie *http.Cookie) browserSessionClaims {
	t.Helper()
	var claims browserSessionClaims
	if err := s.browserSessionCodec().Decode(cookie.Name, cookie.Value, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}
func renewalCookie(t *testing.T, s *Store, cookie *http.Cookie, claims browserSessionClaims) *http.Cookie {
	t.Helper()
	out := *cookie
	var err error
	out.Value, err = s.browserSessionCodec().Encode(out.Name, claims)
	if err != nil {
		t.Fatal(err)
	}
	return &out
}
func TestBrowserSessionRenewalOriginalCapabilityCeilingAndCurrentGrants(t *testing.T) {
	for _, kind := range []string{"readonly-promoted", "writer-promoted", "admin-demoted"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := streamFixture(t)
			if kind == "admin-demoted" {
				if _, err := s.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='credential'`); err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest("POST", "http://localhost/api/v1/projects/project/browser-session", nil)
			r.SetPathValue("project_id", "project")
			r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
			r.Header.Set("X-FC-Instance", s.InstanceID())
			id := strings.Repeat("a", 32)
			_, cookie, err := s.CreateBrowserSession(r, id, kind != "readonly-promoted")
			if err != nil {
				t.Fatal(err)
			}
			original := renewalClaims(t, s, cookie)
			original.ExpiresAt = time.Now().Add(time.Hour).Unix()
			cookie = renewalCookie(t, s, cookie, original)
			if kind == "admin-demoted" {
				_, err = s.DB().Exec(`UPDATE credentials SET is_admin=0 WHERE id='credential';UPDATE grants SET can_write=0 WHERE credential_id='credential'`)
			} else {
				_, err = s.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='credential'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now().UTC().Truncate(time.Second)
			response, next, err := s.RenewBrowserSession(renewalCoreRequest(s, id, cookie))
			if err != nil {
				t.Fatal(err)
			}
			claims := renewalClaims(t, s, next)
			if response.SessionID != id || response.ProjectID != "project" || response.InstanceID != s.InstanceID() || claims.ExpiresAt < before.Add(24*time.Hour).Unix() || claims.ExpiresAt > time.Now().Add(24*time.Hour).Unix() {
				t.Fatal("renewal identity/lifetime", response)
			}
			if next.MaxAge != 86400 || next.Name != cookie.Name || next.Secure != cookie.Secure || !next.HttpOnly || next.SameSite != http.SameSiteStrictMode || next.Domain != "" || next.Path != "/" {
				t.Fatal("renewal weakened selected cookie")
			}
			claims.ExpiresAt = original.ExpiresAt
			if claims != original {
				t.Fatal("renewal changed original encrypted authority")
			}
			expectedWrite := kind == "writer-promoted"
			if response.AllowWrites != expectedWrite || response.CanEditPresentation {
				t.Fatal("renewal promoted or ignored current grants", response)
			}
			current, err := s.CurrentBrowserSession(browserCoreRequest(s, id, next))
			if err != nil || current != response {
				t.Fatal("renewed cookie cannot resume", current, err)
			}
			if kind == "admin-demoted" {
				if _, err = s.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='credential';UPDATE grants SET can_write=1 WHERE credential_id='credential'`); err != nil {
					t.Fatal(err)
				}
				current, err = s.CurrentBrowserSession(browserCoreRequest(s, id, next))
				if err != nil || !current.AllowWrites || !current.CanEditPresentation {
					t.Fatal("original pins were replaced with transient effective grants", err)
				}
			}
		})
	}
}
func TestBrowserSessionRenewalDeniesExpiredRevokedAndForeign(t *testing.T) {
	for _, kind := range []string{"expired", "expiry-boundary", "revoked", "read-revoked", "foreign-project", "foreign-instance", "tampered", "bearer", "missing-origin"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := streamFixture(t)
			id, cookie := browserCoreSession(t, s)
			r := renewalCoreRequest(s, id, cookie)
			code := "unauthorized"
			switch kind {
			case "expired", "expiry-boundary":
				claims := renewalClaims(t, s, cookie)
				claims.ExpiresAt = time.Now().Unix()
				if kind == "expired" {
					claims.ExpiresAt--
				}
				r = renewalCoreRequest(s, id, renewalCookie(t, s, cookie, claims))
			case "revoked":
				if _, err := s.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='credential'`); err != nil {
					t.Fatal(err)
				}
			case "read-revoked":
				if _, err := s.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='credential'`); err != nil {
					t.Fatal(err)
				}
				code = "forbidden"
			case "foreign-project":
				r.SetPathValue("project_id", "other")
				r.URL.Path = "/api/v1/projects/other/browser-session/renew"
				code = "forbidden"
			case "foreign-instance":
				r.Header.Set("X-FC-Instance", "instance_other")
				code = "instance_mismatch"
			case "tampered":
				r.Header.Set("Cookie", cookie.Name+"=tampered")
			case "bearer":
				r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
			case "missing-origin":
				r.Header.Del("Origin")
				code = "forbidden"
			}
			_, next, err := s.RenewBrowserSession(r)
			browserCoreCode(t, err, code)
			if next != nil {
				t.Fatal("failed renewal issued cookie")
			}
		})
	}
}
func TestBrowserSessionRenewalClockBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		expiry int64
		valid  bool
	}{{now.Unix() - 1, false}, {now.Unix(), false}, {now.Unix() + 1, true}, {now.Add(browserSessionLifetime + 5*time.Second).Unix(), true}, {now.Add(browserSessionLifetime + 6*time.Second).Unix(), false}} {
		if got := browserSessionExpiryValid(test.expiry, now); got != test.valid {
			t.Fatal("fixed clock expiry boundary", test, got)
		}
	}
}
