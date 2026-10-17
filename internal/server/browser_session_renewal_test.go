package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionRenewalPublicCookieIsolationStrictnessAndNoEffects(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/one")
	other := f.project(t, "owner/two")
	token := "synthetic-renewal-project-token-1234567"
	f.grant(t, project, token, false)
	for _, host := range []string{"localhost", "preview.example"} {
		t.Run(host, func(t *testing.T) {
			id := strings.Repeat("a", 32)
			secondID := strings.Repeat("b", 32)
			cookie := browserLogin(t, f, project, token, id, host)
			second := browserLoginCapabilities(t, f, other, f.admin, secondID, host, true, true)
			// Login admission normally limits cookies to eight. Renewal must
			// replace only its selected cookie even if a caller supplies more.
			var additional []*http.Cookie
			for _, digit := range []string{"c", "d", "e", "f", "1", "2", "3"} {
				additional = append(additional, browserLogin(t, f, project, token, strings.Repeat(digit, 32), host))
			}
			events, receipts, providers := count(t, f.store, "events"), count(t, f.store, "receipts"), f.calls.Load()
			origin := "https://" + host
			if host == "localhost" {
				origin = "http://" + host
			}
			makeRequest := func(body string) *http.Request {
				r := httptest.NewRequest("POST", "http://"+host+"/api/v1/projects/"+project+"/browser-session/renew", strings.NewReader(body))
				r.Header.Set("Origin", origin)
				r.Header.Set("X-FC-Instance", f.store.InstanceID())
				r.Header.Set("X-FC-Browser-Session", id)
				r.AddCookie(cookie)
				r.AddCookie(second)
				for _, extra := range additional {
					r.AddCookie(extra)
				}
				return r
			}
			before := time.Now().UTC().Truncate(time.Second)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, makeRequest(`{}`))
			v := require(t, w, 200)
			if len(v) != 6 || v["session_id"] != id || v["project_id"] != project || v["instance_id"] != f.store.InstanceID() || v["allow_writes"] != false || v["can_edit_presentation"] != false {
				t.Fatal("renewal six-field response")
			}
			expires, err := time.Parse(time.RFC3339, v["expires_at"].(string))
			if err != nil || expires.Before(before.Add(24*time.Hour)) || expires.After(time.Now().Add(24*time.Hour)) {
				t.Fatal("rolling lifetime", err)
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != cookie.Name || cookies[0].Name == second.Name || cookies[0].Secure != cookie.Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" || cookies[0].Domain != "" || cookies[0].MaxAge != 86400 {
				t.Fatal("renewal changed another tab or cookie security")
			}
			if strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), cookies[0].Value) {
				t.Fatal("renewal exposed credential/cookie")
			}
			require(t, browserRequest(f, "GET", "/api/v1/projects/"+project, "", id, "", host, cookies[0], second), 200)
			require(t, browserRequest(f, "GET", "/api/v1/projects/"+other, "", secondID, "", host, cookies[0], second), 200)
			for _, body := range []string{`null`, `[]`, `{"allow_writes":true}`, `{"session_id":"` + id + `"}`, `{"x":1,"x":1}`, `{} {}`} {
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, makeRequest(body))
				require(t, w, 400)
				if len(w.Result().Cookies()) != 0 {
					t.Fatal("invalid body changed cookie")
				}
			}
			for _, kind := range []string{"missing-origin", "foreign-origin", "duplicate-origin", "cross-site", "bearer", "selector-missing", "selector-duplicate", "cookie-missing", "cookie-duplicate", "cookie-tampered", "wrong-instance", "missing-instance", "foreign-project", "query"} {
				t.Run(kind, func(t *testing.T) {
					r := makeRequest(`{}`)
					status := 401
					switch kind {
					case "missing-origin":
						r.Header.Del("Origin")
						status = 403
					case "foreign-origin":
						r.Header.Set("Origin", "https://foreign.example")
						status = 403
					case "duplicate-origin":
						r.Header.Add("Origin", origin)
						status = 403
					case "cross-site":
						r.Header.Set("Sec-Fetch-Site", "cross-site")
						status = 403
					case "bearer":
						r.Header.Set("Authorization", "Bearer "+token)
					case "selector-missing":
						r.Header.Del("X-FC-Browser-Session")
					case "selector-duplicate":
						r.Header.Add("X-FC-Browser-Session", id)
					case "cookie-missing":
						r.Header.Del("Cookie")
					case "cookie-duplicate":
						r.AddCookie(cookie)
					case "cookie-tampered":
						r.Header.Set("Cookie", cookie.Name+"=tampered")
					case "wrong-instance":
						r.Header.Set("X-FC-Instance", "instance_wrong")
						status = 409
					case "missing-instance":
						r.Header.Del("X-FC-Instance")
						status = 400
					case "foreign-project":
						r.URL.Path = "/api/v1/projects/" + other + "/browser-session/renew"
						status = 403
					case "query":
						r.URL.RawQuery = "unexpected=1"
						status = 400
					}
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, r)
					require(t, w, status)
					if len(w.Result().Cookies()) != 0 {
						t.Fatal("rejected renewal altered any cookie")
					}
				})
			}
			if count(t, f.store, "events") != events || count(t, f.store, "receipts") != receipts || f.calls.Load() != providers {
				t.Fatal("renewal changed durable state or called provider")
			}
		})
	}
}
