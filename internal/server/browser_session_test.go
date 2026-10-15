package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

func browserRequest(f *fixture, method, path, token, id, body, host string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if id != "" {
		r.Header.Set("X-FC-Browser-Session", id)
	}
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func browserLogin(t *testing.T, f *fixture, project, token, id, host string) *http.Cookie {
	t.Helper()
	w := browserRequest(f, "POST", "/api/v1/projects/"+project+"/browser-session", token, "", `{"session_id":"`+id+`"}`, host)
	v := require(t, w, 200)
	if len(v) != 4 || v["session_id"] != id || v["project_id"] != project || v["instance_id"] != f.store.InstanceID() {
		t.Fatal("session response shape mismatch")
	}
	if expiry, err := time.Parse(time.RFC3339, v["expires_at"].(string)); err != nil || time.Until(expiry) < 23*time.Hour || time.Until(expiry) > 24*time.Hour {
		t.Fatal("session expiry invalid")
	}
	if strings.Contains(w.Body.String(), token) {
		t.Fatal("session response exposed bearer")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" || cookies[0].Domain != "" || cookies[0].MaxAge != 86400 || len(cookies[0].Value) > 4096 {
		t.Fatal("cookie flags invalid")
	}
	if strings.Contains(w.Body.String(), cookies[0].Value) {
		t.Fatal("session response exposed cookie")
	}
	return cookies[0]
}

func TestBrowserSessionPublicReadOnlyIsolationAndRestart(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	other := f.project(t, "owner/two")
	read, write := "synthetic-browser-read-token-1234567", "synthetic-browser-write-token-1234567"
	f.grant(t, p, read, false)
	f.grant(t, p, write, true)
	events, receipts, calls := count(t, f.store, "events"), count(t, f.store, "receipts"), f.calls.Load()
	for i, token := range []string{read, write, f.admin} {
		id := strings.Repeat(string(rune('a'+i)), 32)
		cookie := browserLogin(t, f, p, token, id, "localhost")
		if cookie.Secure || cookie.Name != "fc-local-browser-"+id {
			t.Fatal("loopback HTTP cookie flags invalid")
		}
		require(t, browserRequest(f, "GET", "/api/v1/projects/"+p, "", id, "", "localhost", cookie), 200)
		require(t, browserRequest(f, "HEAD", "/api/v1/projects/"+p, "", id, "", "localhost", cookie), 200)
		require(t, browserRequest(f, "GET", "/api/v1/instance", "", id, "", "localhost", cookie), 200)
		for _, path := range []string{"/api/v1/projects", "/api/v1/projects/" + other} {
			require(t, browserRequest(f, "GET", path, "", id, "", "localhost", cookie), 403)
		}
		require(t, browserRequest(f, "POST", "/api/v1/projects/"+p+"/workers", "", id, `{"name":"blocked"}`, "localhost", cookie), 403)
		require(t, browserRequest(f, "POST", "/api/v1/projects/"+p+"/browser-session", "", id, `{"session_id":"`+id+`"}`, "localhost", cookie), 401)
	}
	id1, id2 := strings.Repeat("d", 32), strings.Repeat("e", 32)
	c1 := browserLogin(t, f, p, read, id1, "localhost")
	c2 := browserLogin(t, f, other, f.admin, id2, "localhost")
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+p, "", id1, "", "localhost", c1, c2), 200)
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+p, "", id2, "", "localhost", c1, c2), 403)
	w := browserRequest(f, "DELETE", "/api/v1/projects/"+p+"/browser-session", "", id1, `{}`, "localhost", c1, c2)
	v := require(t, w, 200)
	if len(v) != 2 || v["session_id"] != id1 || v["logged_out"] != true {
		t.Fatal("logout response mismatch")
	}
	clear := w.Result().Cookies()
	if len(clear) != 1 || clear[0].Name != c1.Name || clear[0].MaxAge != -1 {
		t.Fatal("logout cleared other tab")
	}
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+other, "", id2, "", "localhost", c2), 200)
	// Logout also succeeds when the expired cookie is no longer sent by a browser.
	require(t, browserRequest(f, "DELETE", "/api/v1/projects/"+p+"/browser-session", "", id1, `{}`, "localhost"), 200)
	if count(t, f.store, "events") != events || count(t, f.store, "receipts") != receipts || f.calls.Load() != calls {
		t.Fatal("session transport changed domain state or called provider")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = core.Open(context.Background(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = server.New(f.store)
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+p, "", id1, "", "localhost", c1), 200)
	// Server-side session deletion is deliberately not a revocation of bearer access.
	require(t, request(f.handler, "GET", "/api/v1/projects/"+p, read, "", "", ""), 200)
}

func TestBrowserSessionPublicCredentialRevocation(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	token := "synthetic-browser-revoked-token-1234567"
	credential := f.grant(t, p, token, false)
	id := strings.Repeat("f", 32)
	cookie := browserLogin(t, f, p, token, id, "localhost")
	require(t, browserRequest(f, "GET", "/api/v1/instance", "", id, "", "localhost", cookie), 200)
	require(t, request(f.handler, "DELETE", "/api/v1/projects/"+p+"/credentials/"+credential, f.admin, core.NewID("key"), "", `{}`), 200)
	require(t, browserRequest(f, "GET", "/api/v1/instance", "", id, "", "localhost", cookie), 401)
	require(t, browserRequest(f, "GET", "/api/v1/projects/"+p, "", id, "", "localhost", cookie), 401)
}

func TestBrowserSessionStrictOriginProxyAndAuthenticationFailures(t *testing.T) {
	f := setup(t)
	p := f.project(t, "owner/one")
	id := strings.Repeat("a", 32)
	proxyCookie := browserLogin(t, f, p, f.admin, id, "preview.example")
	if !proxyCookie.Secure || proxyCookie.Name != "__Host-fc-browser-"+id {
		t.Fatal("public proxy weakened cookie")
	}
	for _, body := range []string{`{}`, `null`, `{"session_id":null}`, `{"session_id":"` + id + `","extra":true}`, `{"Session_ID":"` + id + `"}`, `{"session_id":"` + id + `","session_id":"` + id + `"}`, `{"session_id":"UPPER"}`} {
		require(t, browserRequest(f, "POST", "/api/v1/projects/"+p+"/browser-session", f.admin, "", body, "localhost"), 400)
	}
	for _, name := range []string{"cross_origin", "cross_site", "public_http", "forwarded", "missing_instance", "wrong_instance", "duplicate_instance", "selector", "missing_bearer"} {
		t.Run(name, func(t *testing.T) {
			host := "localhost"
			if name == "public_http" || name == "forwarded" {
				host = "preview.example"
			}
			r := httptest.NewRequest("POST", "http://"+host+"/api/v1/projects/"+p+"/browser-session", strings.NewReader(`{"session_id":"`+id+`"}`))
			r.Header.Set("Authorization", "Bearer "+f.admin)
			r.Header.Set("X-FC-Instance", f.store.InstanceID())
			status := 403
			switch name {
			case "cross_origin":
				r.Header.Set("Origin", "https://other.example")
			case "cross_site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "public_http":
				r.Header.Set("Origin", "http://preview.example")
			case "forwarded":
				r.Header.Set("Origin", "http://preview.example")
				r.Header.Set("X-Forwarded-Host", "localhost")
				r.Header.Set("X-Forwarded-Proto", "http")
			case "missing_instance":
				r.Header.Del("X-FC-Instance")
				status = 400
			case "wrong_instance":
				r.Header.Set("X-FC-Instance", "instance_other")
				status = 409
			case "duplicate_instance":
				r.Header.Add("X-FC-Instance", f.store.InstanceID())
				status = 400
			case "selector":
				r.Header.Set("X-FC-Browser-Session", id)
				status = 401
			case "missing_bearer":
				r.Header.Del("Authorization")
				status = 401
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			require(t, w, status)
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("failed login set a cookie")
			}
		})
	}
	// HTTPS Origin is valid behind a public proxy while forwarding is ignored.
	r := httptest.NewRequest("POST", "http://preview.example/api/v1/projects/"+p+"/browser-session", strings.NewReader(`{"session_id":"`+id+`"}`))
	r.Header.Set("Origin", "https://preview.example")
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	require(t, w, 200)
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("proxy cookie not secure")
	}
	cookie := browserLogin(t, f, p, f.admin, id, "localhost")
	for _, name := range []string{"ambient", "bad_bearer", "ambiguous", "duplicate_cookie", "duplicate_selector", "tamper"} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://localhost/api/v1/projects/"+p, nil)
			r.AddCookie(cookie)
			r.Header.Set("X-FC-Browser-Session", id)
			switch name {
			case "ambient":
				r.Header.Del("X-FC-Browser-Session")
			case "bad_bearer":
				r.Header.Del("X-FC-Browser-Session")
				r.Header.Set("Authorization", "Bearer invalid")
			case "ambiguous":
				r.Header.Set("Authorization", "Bearer "+f.admin)
			case "duplicate_cookie":
				r.AddCookie(cookie)
			case "duplicate_selector":
				r.Header.Add("X-FC-Browser-Session", id)
			case "tamper":
				r.Header.Set("Cookie", cookie.Name+"=invalid")
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			require(t, w, 401)
		})
	}
	for _, name := range []string{"cross_origin", "cross_site", "missing_selector", "body", "authorization"} {
		t.Run("logout_"+name, func(t *testing.T) {
			body := `{}`
			if name == "body" {
				body = `{"extra":true}`
			}
			r := httptest.NewRequest("DELETE", "http://localhost/api/v1/projects/"+p+"/browser-session", strings.NewReader(body))
			r.Header.Set("X-FC-Browser-Session", id)
			status := 403
			switch name {
			case "cross_origin":
				r.Header.Set("Origin", "http://other.example")
			case "cross_site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "missing_selector":
				r.Header.Del("X-FC-Browser-Session")
				status = 400
			case "body":
				status = 400
			case "authorization":
				r.Header.Set("Authorization", "Bearer "+f.admin)
				status = 401
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			require(t, w, status)
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("failed logout cleared cookie")
			}
		})
	}
}

func TestBrowserSessionReadsRequireOneExpectedInstanceWithoutChangingBearerBootstrap(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one") // Explicit bearer bootstrap omits instance binding.
	token := "synthetic-bound-browser-read-token-1234567"
	f.grant(t, project, token, false)
	id := strings.Repeat("b", 32)
	cookie := browserLogin(t, f, project, token, id, "localhost")
	events, receipts, calls := count(t, f.store, "events"), count(t, f.store, "receipts"), f.calls.Load()
	for _, endpoint := range []struct{ method, path string }{
		{"GET", "/api/v1/projects/" + project}, {"HEAD", "/api/v1/projects/" + project},
		{"GET", "/api/v1/instance"}, {"HEAD", "/api/v1/instance"},
	} {
		for _, check := range []struct {
			name   string
			values []string
			status int
			code   string
		}{
			{"missing", nil, 400, "validation_failed"},
			{"empty", []string{""}, 400, "validation_failed"},
			{"duplicate", []string{f.store.InstanceID(), f.store.InstanceID()}, 400, "validation_failed"},
			{"wrong", []string{"instance_other"}, 409, "instance_mismatch"},
			{"invalid", []string{"malformed instance"}, 400, "validation_failed"},
			{"valid", []string{f.store.InstanceID()}, 200, ""},
		} {
			t.Run(endpoint.method+endpoint.path+"/"+check.name, func(t *testing.T) {
				r := httptest.NewRequest(endpoint.method, "http://localhost"+endpoint.path, nil)
				r.Header.Set("X-FC-Browser-Session", id)
				for _, value := range check.values {
					r.Header.Add("X-FC-Instance", value)
				}
				r.AddCookie(cookie)
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				body := require(t, w, check.status)
				if check.code != "" {
					failure, ok := body["error"].(map[string]any)
					if !ok || failure["code"] != check.code {
						t.Fatal("wrong binding failure classification", body)
					}
				}
			})
		}
	}
	// This exception belongs to bearer bootstrap, never to session transport.
	for _, path := range []string{"/api/v1/instance", "/api/v1/projects"} {
		require(t, request(f.handler, "GET", path, f.admin, "", "", ""), 200)
	}
	if count(t, f.store, "events") != events || count(t, f.store, "receipts") != receipts || f.calls.Load() != calls {
		t.Fatal("binding checks changed domain state or called provider")
	}
}
