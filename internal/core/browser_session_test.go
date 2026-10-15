package core

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func browserCoreSession(t *testing.T, s *Store) (string, *http.Cookie) {
	t.Helper()
	id := strings.Repeat("a", 32)
	r := httptest.NewRequest("POST", "http://localhost/api/v1/projects/project/browser-session", nil)
	r.SetPathValue("project_id", "project")
	r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
	r.Header.Set("X-FC-Instance", s.InstanceID())
	response, cookie, err := s.CreateBrowserSession(r, id)
	if err != nil || response.ProjectID != "project" || response.SessionID != id {
		t.Fatal("create session failed", err)
	}
	return id, cookie
}

func browserCoreRequest(id string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "http://localhost/api/v1/projects/project", nil)
	r.SetPathValue("project_id", "project")
	r.Header.Set("X-FC-Browser-Session", id)
	r.Header.Set("X-FC-Worker", "worker")
	r.AddCookie(cookie)
	return r
}

func browserCoreCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func TestBrowserSessionClaimsReadOnlyAndReauthorization(t *testing.T) {
	s, _, _ := streamFixture(t)
	id, cookie := browserCoreSession(t, s)
	scope, err := s.Scope(browserCoreRequest(id, cookie), "read")
	if err != nil || !scope.readOnly || scope.sessionProject != "project" || scope.tokenHash == "" || scope.admin {
		t.Fatal("cookie scope lost restriction", err)
	}
	_, err = s.Mutate(context.Background(), scope, Mutation{Operation: "test", Target: "test", Key: "blocked-write", Request: struct{}{}}, func(context.Context, *sql.Tx) (any, error) {
		t.Fatal("session entered mutation callback")
		return nil, nil
	})
	browserCoreCode(t, err, "forbidden")
	forged := scope
	forged.ProjectID = "another-project"
	err = s.Read(context.Background(), forged, func(context.Context, *sql.Tx) error { t.Fatal("rebound session entered read"); return nil })
	browserCoreCode(t, err, "forbidden")
	expired := scope
	expired.expiresAt = time.Now().Add(-time.Second)
	err = s.Read(context.Background(), expired, func(context.Context, *sql.Tx) error { t.Fatal("expired session entered read"); return nil })
	browserCoreCode(t, err, "unauthorized")
	if _, err := s.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='credential'`); err != nil {
		t.Fatal(err)
	}
	err = s.Read(context.Background(), scope, func(context.Context, *sql.Tx) error { return nil })
	browserCoreCode(t, err, "forbidden")
	instance := browserCoreRequest(id, cookie)
	instance.SetPathValue("project_id", "")
	instance.URL.Path = "/api/v1/instance"
	_, err = s.Authenticate(instance)
	browserCoreCode(t, err, "forbidden")
	if _, err := s.DB().Exec(`UPDATE grants SET can_read=1 WHERE credential_id='credential';UPDATE credentials SET revoked=1 WHERE id='credential'`); err != nil {
		t.Fatal(err)
	}
	err = s.Read(context.Background(), scope, func(context.Context, *sql.Tx) error { return nil })
	browserCoreCode(t, err, "unauthorized")
}

func TestBrowserSessionRejectsInvalidClaimsAndTransport(t *testing.T) {
	s, _, _ := streamFixture(t)
	id, cookie := browserCoreSession(t, s)
	var original browserSessionClaims
	if err := s.browserSessionCodec().Decode(cookie.Name, cookie.Value, &original); err != nil {
		t.Fatal("fixture decode failed")
	}
	for name, change := range map[string]func(*browserSessionClaims){
		"expired":  func(c *browserSessionClaims) { c.ExpiresAt = time.Now().Add(-time.Second).Unix() },
		"future":   func(c *browserSessionClaims) { c.ExpiresAt = time.Now().Add(25 * time.Hour).Unix() },
		"nonce":    func(c *browserSessionClaims) { c.SessionID = strings.Repeat("b", 32) },
		"instance": func(c *browserSessionClaims) { c.InstanceID = "instance_other" },
		"project":  func(c *browserSessionClaims) { c.ProjectID = "bad/project" },
		"hash":     func(c *browserSessionClaims) { c.CredentialHash = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			claims := original
			change(&claims)
			c := *cookie
			var err error
			c.Value, err = s.browserSessionCodec().Encode(c.Name, claims)
			if err != nil {
				t.Fatal("fixture encode failed")
			}
			_, err = s.Scope(browserCoreRequest(id, &c), "read")
			browserCoreCode(t, err, "unauthorized")
		})
	}
	for _, name := range []string{"tamper", "duplicate_cookie", "duplicate_selector", "no_selector", "bad_bearer", "ambiguity", "wrong_name", "write", "admin", "listing"} {
		t.Run(name, func(t *testing.T) {
			r := browserCoreRequest(id, cookie)
			code := "unauthorized"
			switch name {
			case "tamper":
				r.Header.Set("Cookie", cookie.Name+"=invalid")
			case "duplicate_cookie":
				r.AddCookie(cookie)
			case "duplicate_selector":
				r.Header.Add("X-FC-Browser-Session", id)
			case "no_selector":
				r.Header.Del("X-FC-Browser-Session")
			case "bad_bearer":
				r.Header.Del("X-FC-Browser-Session")
				r.Header.Set("Authorization", "Bearer invalid")
			case "ambiguity":
				r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
			case "wrong_name":
				r.Header.Set("Cookie", "__Host-fc-browser-"+id+"="+cookie.Value)
			case "write":
				r.Method = "POST"
				code = "forbidden"
			case "admin":
				_, err := s.AdminScope(r)
				browserCoreCode(t, err, "forbidden")
				return
			case "listing":
				r.SetPathValue("project_id", "")
				r.URL.Path = "/api/v1/projects"
				code = "forbidden"
			}
			_, err := s.Authenticate(r)
			browserCoreCode(t, err, code)
		})
	}
}

func TestBrowserSessionStreamReauthorizesExpiryRevocationAndGrants(t *testing.T) {
	for _, name := range []string{"expiry", "revoked", "grant"} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := streamFixture(t)
			id, cookie := browserCoreSession(t, s)
			scope, err := s.Scope(browserCoreRequest(id, cookie), "read")
			if err != nil {
				t.Fatal(err)
			}
			if name == "expiry" {
				scope.expiresAt = time.Now().Add(100 * time.Millisecond)
			}
			cursor := s.Cursor(scope, messageFeed+scope.WorkerID, "0")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			delivered := 0
			err = s.streamMessageEvents(ctx, scope, cursor, 1, func(EventPage) error {
				delivered++
				if name == "revoked" {
					_, err := s.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='credential'`)
					return err
				}
				if name == "grant" {
					_, err := s.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='credential'`)
					return err
				}
				return nil
			}, func() error { return nil }, 5*time.Millisecond)
			code := "unauthorized"
			if name == "grant" {
				code = "forbidden"
			}
			browserCoreCode(t, err, code)
			if delivered != 1 || streamSlots(s) != 0 {
				t.Fatal("session invalidation leaked waiter or delivered after invalidation")
			}
		})
	}
}

func TestBrowserSessionLocalTransportRequiresLiteralLoopback(t *testing.T) {
	for _, host := range []string{"localhost", "localhost:41574", "127.0.0.1:41574", "127.12.1.2:80", "[::1]:41574"} {
		if !BrowserSessionLocalHTTP(httptest.NewRequest("GET", "http://"+host+"/", nil)) {
			t.Fatal("literal loopback did not allow HTTP")
		}
	}
	for _, target := range []string{"https://localhost/", "http://localhost.example/", "http://app.localhost/", "http://preview.example/", "http://192.168.0.1/"} {
		r := httptest.NewRequest("GET", target, nil)
		r.Header.Set("X-Forwarded-Host", "localhost")
		r.Header.Set("X-Forwarded-Proto", "http")
		if BrowserSessionLocalHTTP(r) {
			t.Fatal("TLS, nonliteral or public transport allowed insecure cookie")
		}
	}
}
