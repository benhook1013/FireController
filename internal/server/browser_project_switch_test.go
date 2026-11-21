package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserProjectSwitchHTTPOptInLegacyIsolationAndNoEffects(t *testing.T) {
	f := setup(t)
	project, other := f.project(t, "owner/one"), f.project(t, "owner/two")
	worker := f.worker(t, project, "Source-only worker")
	token := "synthetic-switching-native-token-1234567"
	credential := f.grant(t, project, token, true)
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES(?,?,1,0,0)`, credential, other); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"localhost", "preview.example"} {
		t.Run(host, func(t *testing.T) {
			id, nextID := strings.Repeat("a", 32), strings.Repeat("b", 32)
			for _, flag := range []string{"", `,"project_switching":false`} {
				w := browserRequest(f, "POST", "/api/v1/projects/"+project+"/browser-session", token, "", `{"session_id":"`+id+`"`+flag+`}`, host)
				out := require(t, w, 200)
				if len(out) != 6 || out["projects"] != nil {
					t.Fatal("non-opt-in response changed")
				}
			}
			w := browserRequest(f, "POST", "/api/v1/projects/"+project+"/browser-session", token, "", `{"session_id":"`+id+`","project_switching":true}`, host)
			out := require(t, w, 200)
			projects, ok := out["projects"].([]any)
			if !ok || len(projects) != 2 || len(out) != 7 || projects[0].(map[string]any)["id"] != project {
				t.Fatal("missing opt-in project list")
			}
			for _, entry := range projects {
				project := entry.(map[string]any)
				if len(project) != 3 || project["handle"] == "" || project["name"] == "" {
					t.Fatal("unexpected selectable metadata")
				}
			}
			cookie := w.Result().Cookies()[0]
			events, receipts, providers := count(t, f.store, "events"), count(t, f.store, "receipts"), f.calls.Load()
			origin := "https://" + host
			if host == "localhost" {
				origin = "http://" + host
			}
			makeRequest := func(source, selector, body string, selected *http.Cookie) *http.Request {
				r := httptest.NewRequest("POST", "http://"+host+"/api/v1/projects/"+source+"/browser-session/switch", strings.NewReader(body))
				r.Header.Set("X-FC-Instance", f.store.InstanceID())
				r.Header.Set("X-FC-Browser-Session", selector)
				r.Header.Set("Origin", origin)
				r.AddCookie(selected)
				return r
			}
			body := `{"project_id":"` + other + `","session_id":"` + nextID + `"}`
			r := makeRequest(project, id, body, cookie)
			r.Header.Set("X-FC-Worker", worker)
			w = httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			switched := require(t, w, 200)
			if switched["project_id"] != other || switched["session_id"] != nextID || switched["expires_at"] != out["expires_at"] || switched["allow_writes"] != false || switched["can_edit_presentation"] != false || len(switched["projects"].([]any)) != 2 {
				t.Fatal("switch response changed target ceilings, lifetime or original list")
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name == cookie.Name || cookies[0].MaxAge <= 0 || cookies[0].MaxAge > 86400 || cookies[0].Secure != (host != "localhost") || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" || cookies[0].Domain != "" {
				t.Fatal("switch weakened cookie security or deleted source")
			}
			targetCookie := cookies[0]
			if strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), targetCookie.Value) {
				t.Fatal("switch exposed credential material")
			}
			for _, source := range []struct {
				project, selector string
				cookie            *http.Cookie
			}{{project, id, cookie}, {other, nextID, targetCookie}} {
				current := require(t, browserRequest(f, "GET", "/api/v1/projects/"+source.project+"/browser-session", "", source.selector, "", host, source.cookie), 200)
				if len(current["projects"].([]any)) != 2 {
					t.Fatal("current response lost original selectable list")
				}
			}
			require(t, browserRequest(f, "GET", "/api/v1/projects/"+other, "", id, "", host, cookie, targetCookie), 403)
			require(t, browserRequest(f, "GET", "/api/v1/projects/"+project, "", nextID, "", host, cookie, targetCookie), 403)
			r = makeRequest(other, nextID, `{}`, targetCookie)
			r.URL.Path = "/api/v1/projects/" + other + "/browser-session/renew"
			w = httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			renewed := require(t, w, 200)
			if len(renewed["projects"].([]any)) != 2 || renewed["allow_writes"] != false {
				t.Fatal("renewal lost selected target ceiling/list")
			}
			// The former reader can switch back to the original writer without
			// gaining management, or carrying the source worker to the target.
			r = makeRequest(other, nextID, `{"project_id":"`+project+`","session_id":"`+strings.Repeat("c", 32)+`"}`, targetCookie)
			w = httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			back := require(t, w, 200)
			if back["allow_writes"] != true || back["can_edit_presentation"] != false {
				t.Fatal("round trip did not retain original target ceiling")
			}
			if count(t, f.store, "events") != events || count(t, f.store, "receipts") != receipts || f.calls.Load() != providers {
				t.Fatal("switch changed durable domain state or called provider")
			}
		})
	}
}

func TestBrowserProjectSwitchHTTPStrictBodyOriginAuthenticationAndSelectors(t *testing.T) {
	f := setup(t)
	project, other := f.project(t, "owner/one"), f.project(t, "owner/two")
	id, nextID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	login := browserRequest(f, "POST", "/api/v1/projects/"+project+"/browser-session", f.admin, "", `{"session_id":"`+id+`","project_switching":true}`, "localhost")
	require(t, login, 200)
	cookie := login.Result().Cookies()[0]
	body := `{"project_id":"` + other + `","session_id":"` + nextID + `"}`
	makeRequest := func(body string) *http.Request {
		r := httptest.NewRequest("POST", "http://localhost/api/v1/projects/"+project+"/browser-session/switch", strings.NewReader(body))
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		r.Header.Set("X-FC-Browser-Session", id)
		r.AddCookie(cookie)
		return r
	}
	for i, invalid := range []string{`null`, `[]`, `{}`, `{"project_id":"` + other + `"}`, `{"project_id":"` + other + `","session_id":"UPPERCASE"}`, `{"project_id":"` + other + `","session_id":"` + nextID + `","worker_id":"worker"}`, `{"project_id":"` + other + `","project_id":"` + other + `","session_id":"` + nextID + `"}`, body + ` {}`} {
		t.Run(fmt.Sprintf("body-%d", i), func(t *testing.T) {
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, makeRequest(invalid))
			require(t, w, 400)
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("invalid body altered cookie")
			}
		})
	}
	for _, kind := range []string{"missing-origin", "foreign-origin", "duplicate-origin", "cross-site", "bearer", "bearer-only", "selector-missing", "selector-duplicate", "cookie-missing", "cookie-duplicate", "cookie-tampered", "wrong-instance", "missing-instance", "foreign-source", "current-target", "unknown-target", "handle-target", "same-selector", "used-selector", "query"} {
		t.Run(kind, func(t *testing.T) {
			r := makeRequest(body)
			status := 401
			switch kind {
			case "missing-origin":
				r.Header.Del("Origin")
				status = 403
			case "foreign-origin":
				r.Header.Set("Origin", "https://foreign.example")
				status = 403
			case "duplicate-origin":
				r.Header.Add("Origin", "http://localhost")
				status = 403
			case "cross-site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				status = 403
			case "bearer", "bearer-only":
				r.Header.Set("Authorization", "Bearer "+f.admin)
				if kind == "bearer-only" {
					r.Header.Del("X-FC-Browser-Session")
				}
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
				r.Header.Set("X-FC-Instance", "foreign-instance")
				status = 409
			case "missing-instance":
				r.Header.Del("X-FC-Instance")
				status = 400
			case "foreign-source":
				r.URL.Path = "/api/v1/projects/" + other + "/browser-session/switch"
				status = 403
			case "current-target":
				r = makeRequest(`{"project_id":"` + project + `","session_id":"` + nextID + `"}`)
				status = 400
			case "unknown-target":
				r = makeRequest(`{"project_id":"foreign-project","session_id":"` + nextID + `"}`)
				status = 403
			case "handle-target":
				handle, err := f.store.ProjectHandle(t.Context(), other)
				if err != nil {
					t.Fatal(err)
				}
				r = makeRequest(`{"project_id":"` + handle + `","session_id":"` + nextID + `"}`)
				status = 403
			case "same-selector":
				r = makeRequest(`{"project_id":"` + other + `","session_id":"` + id + `"}`)
				status = 400
			case "used-selector":
				r.AddCookie(&http.Cookie{Name: "fc-local-browser-" + nextID, Value: "already-used"})
				status = 409
			case "query":
				r.URL.RawQuery = "extra=1"
				status = 400
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			require(t, w, status)
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("denied switch altered cookies")
			}
		})
	}
	for _, value := range []string{`null`, `"true"`, `1`, `[]`, `{}`} {
		w := browserRequest(f, "POST", "/api/v1/projects/"+project+"/browser-session", f.admin, "", `{"session_id":"`+id+`","project_switching":`+value+`}`, "localhost")
		require(t, w, 400)
	}
}
