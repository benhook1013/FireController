package server_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBrowserSessionRepeatedTabConnectionsKeepCookieHeaderBounded(t *testing.T) {
	f := setup(t)
	project := f.project(t, "owner/cookie-growth")
	token := "synthetic-browser-cookie-budget-token-1234567"
	f.grant(t, project, token, false)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	target, _ := url.Parse(server.URL)
	path := server.URL + "/api/v1/projects/" + project
	for i := 1; i <= 40; i++ {
		id := fmt.Sprintf("%032x", i)
		request, err := http.NewRequest("POST", path+"/browser-session", strings.NewReader(`{"session_id":"`+id+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-FC-Instance", f.store.InstanceID())
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("new tab could not connect", i, response.StatusCode)
		}
		cookies := jar.Cookies(target)
		bytes := 0
		for i, cookie := range cookies {
			bytes += len(cookie.Name) + len(cookie.Value) + 1
			if i > 0 {
				bytes += 2
			}
		}
		if len(cookies) > 8 || bytes > 6<<10 {
			t.Fatal("closed tabs accumulated unbounded cookies", i, len(cookies), bytes)
		}
		read, _ := http.NewRequest("GET", path, nil)
		read.Header.Set("X-FC-Instance", f.store.InstanceID())
		read.Header.Set("X-FC-Browser-Session", id)
		response, err = client.Do(read)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("cleanup invalidated the new connection", i, response.StatusCode)
		}
	}
}
