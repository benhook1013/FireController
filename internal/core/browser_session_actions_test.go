package core

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func browserCoreWritableSession(t *testing.T, s *Store) (string, *http.Cookie) {
	t.Helper()
	id := strings.Repeat("b", 32)
	r := httptest.NewRequest("POST", "http://localhost/api/v1/projects/project/browser-session", nil)
	r.SetPathValue("project_id", "project")
	r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
	r.Header.Set("X-FC-Instance", s.InstanceID())
	v, cookie, err := s.CreateBrowserSession(r, id, true)
	if err != nil || !v.AllowWrites || v.CanEditPresentation {
		t.Fatal("write session fixture capability mismatch", err)
	}
	return id, cookie
}

func browserCoreWriteRequest(s *Store, id string, cookie *http.Cookie) *http.Request {
	r := browserCoreRequest(s, id, cookie)
	r.Method = "POST"
	r.URL.Path += "/messages"
	r.Header.Set("Origin", "http://localhost")
	return r
}

func TestBrowserSessionWriteScopeReauthorizesInsideMutationAndReplay(t *testing.T) {
	for _, change := range []string{"expiry", "revocation", "write_grant", "read_grant", "project", "instance"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := streamFixture(t)
			id, cookie := browserCoreWritableSession(t, s)
			scope, err := s.Scope(browserCoreWriteRequest(s, id, cookie), "write")
			if err != nil || scope.readOnly || scope.sessionProject != "project" {
				t.Fatal("write scope not retained", err)
			}
			mutation := Mutation{Operation: "test.browser.write", Target: "test", Key: "browser-replay", Request: struct{}{}}
			_, err = s.Mutate(context.Background(), scope, mutation, func(context.Context, *sql.Tx) (any, error) {
				return map[string]string{"outcome": "stored"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			code := "forbidden"
			switch change {
			case "expiry":
				scope.expiresAt = time.Now().Add(-time.Second)
				code = "unauthorized"
			case "revocation":
				_, err = s.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='credential'`)
				code = "unauthorized"
			case "write_grant":
				_, err = s.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id='credential'`)
			case "read_grant":
				_, err = s.DB().Exec(`UPDATE grants SET can_read=0 WHERE credential_id='credential'`)
			case "project":
				scope.ProjectID = "other"
			case "instance":
				scope.instanceID = "instance_other"
				code = "unauthorized"
			}
			if err != nil {
				t.Fatal(err)
			}
			// Both a new write and an exact existing receipt must reauthorize
			// after scope creation, before callback or replay disclosure.
			for _, key := range []string{"browser-replay", "browser-new-write"} {
				mutation.Key = key
				_, err = s.Mutate(context.Background(), scope, mutation, func(context.Context, *sql.Tx) (any, error) {
					t.Fatal("invalidated browser scope entered callback")
					return nil, nil
				})
				browserCoreCode(t, err, code)
			}
		})
	}
}

func TestBrowserSessionOldCookieCannotGainWriteCapability(t *testing.T) {
	s, _, _ := streamFixture(t)
	id, original := browserCoreSession(t, s)
	var claims browserSessionClaims
	if err := s.browserSessionCodec().Decode(original.Name, original.Value, &claims); err != nil {
		t.Fatal("decode fixture")
	}
	// Reproduce the exact older claim shape, rather than an explicit false.
	old := map[string]any{"session_id": claims.SessionID, "instance_id": claims.InstanceID, "project_id": claims.ProjectID, "credential_hash": claims.CredentialHash, "expires_at": claims.ExpiresAt}
	cookie := *original
	var err error
	cookie.Value, err = s.browserSessionCodec().Encode(cookie.Name, old)
	if err != nil {
		t.Fatal("encode fixture")
	}
	read := browserCoreRequest(s, id, &cookie)
	read.URL.Path += "/browser-session"
	current, err := s.CurrentBrowserSession(read)
	if err != nil || current.AllowWrites || current.CanEditPresentation {
		t.Fatal("old cookie gained capability", err)
	}
	_, err = s.Scope(browserCoreWriteRequest(s, id, &cookie), "write")
	browserCoreCode(t, err, "forbidden")
}

func TestBrowserSessionCurrentEffectiveCapabilitiesAndReadAttenuation(t *testing.T) {
	s, _, _ := streamFixture(t)
	id, cookie := browserCoreWritableSession(t, s)
	r := browserCoreRequest(s, id, cookie)
	r.URL.Path += "/browser-session"
	current, err := s.CurrentBrowserSession(r)
	if err != nil || !current.AllowWrites || current.CanEditPresentation {
		t.Fatal("current capabilities incorrect", err)
	}
	_, err = s.Scope(r, "write")
	browserCoreCode(t, err, "forbidden")
	_, err = s.AdminScope(r)
	browserCoreCode(t, err, "forbidden")
	if _, err = s.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id='credential'`); err != nil {
		t.Fatal(err)
	}
	current, err = s.CurrentBrowserSession(r)
	if err != nil || current.AllowWrites || current.CanEditPresentation {
		t.Fatal("current capabilities retained revoked grant", err)
	}
	_, err = s.Scope(browserCoreWriteRequest(s, id, cookie), "write")
	browserCoreCode(t, err, "forbidden")
	if _, err = s.Scope(r, "read"); err != nil {
		t.Fatal("write permission loss broke readable session", err)
	}
}

func TestBrowserSessionWriteCookieStillExpiresDuringReadStream(t *testing.T) {
	s, _, _ := streamFixture(t)
	id, cookie := browserCoreWritableSession(t, s)
	scope, err := s.Scope(browserCoreRequest(s, id, cookie), "read")
	if err != nil || !scope.readOnly {
		t.Fatal("write cookie widened read stream", err)
	}
	scope.expiresAt = time.Now().Add(-time.Second)
	err = s.Read(context.Background(), scope, func(context.Context, *sql.Tx) error {
		t.Fatal("expired write cookie read succeeded")
		return nil
	})
	browserCoreCode(t, err, "unauthorized")
}

func TestBrowserSessionAdminMutationRechecksAuthorityBeforeReceiptReplay(t *testing.T) {
	s, _, _ := streamFixture(t)
	if _, err := s.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='credential'`); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 32)
	login := httptest.NewRequest("POST", "http://localhost/api/v1/projects/project/browser-session", nil)
	login.SetPathValue("project_id", "project")
	login.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
	login.Header.Set("X-FC-Instance", s.InstanceID())
	response, cookie, err := s.CreateBrowserSession(login, id, true)
	if err != nil || !response.CanEditPresentation {
		t.Fatal("administrator session fixture failed", err)
	}
	r := browserCoreWriteRequest(s, id, cookie)
	r.Method = "PUT"
	r.URL.Path = "/api/v1/projects/project/presentation"
	scope, err := s.AdminScope(r)
	if err != nil || !scope.admin || scope.readOnly {
		t.Fatal("administrator cookie scope invalid", err)
	}
	m := Mutation{Operation: "project.presentation.replace", Target: "project", Key: "admin-browser-replay", Request: struct{}{}}
	if _, err = s.Mutate(context.Background(), scope, m, func(context.Context, *sql.Tx) (any, error) { return struct{}{}, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`UPDATE credentials SET is_admin=0 WHERE id='credential'`); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"admin-browser-replay", "admin-browser-new"} {
		m.Key = key
		_, err = s.Mutate(context.Background(), scope, m, func(context.Context, *sql.Tx) (any, error) {
			t.Fatal("demoted browser administrator entered mutation")
			return nil, nil
		})
		browserCoreCode(t, err, "forbidden")
	}
}
