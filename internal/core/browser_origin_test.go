package core

import (
	"net/http/httptest"
	"testing"
)

func TestBrowserSessionOriginRequiresActualExactServingOrigin(t *testing.T) {
	for _, test := range []struct {
		name, host, origin, site string
		required, valid          bool
	}{
		{"optional_absent", "localhost", "", "", false, true},
		{"required_absent", "localhost", "", "", true, false},
		{"loopback", "localhost:41574", "http://localhost:41574", "same-origin", true, true},
		{"loopback_ipv6", "[::1]:41574", "http://[::1]:41574", "same-origin", true, true},
		{"proxy_https", "controller.example", "https://controller.example", "same-origin", true, true},
		{"proxy_http", "controller.example", "http://controller.example", "same-origin", true, false},
		{"port", "localhost:41574", "http://localhost", "same-origin", true, false},
		{"cross_site", "localhost", "http://localhost", "cross-site", true, false},
		{"same_site_other_origin", "controller.example", "https://sibling.example", "same-site", true, false},
		{"null", "localhost", "null", "", true, false},
		{"path", "localhost", "http://localhost/", "", true, false},
		{"user", "localhost", "http://user@localhost", "", true, false},
		{"query", "localhost", "http://localhost?", "", true, false},
		{"fragment", "localhost", "http://localhost#", "", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+test.host+"/", nil)
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			if test.site != "" {
				r.Header.Set("Sec-Fetch-Site", test.site)
			}
			r.Header.Set("X-Forwarded-Host", "localhost")
			r.Header.Set("X-Forwarded-Proto", "http")
			err := BrowserSessionOrigin(r, test.required)
			if (err == nil) != test.valid {
				t.Fatal("origin boundary mismatch", err)
			}
		})
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		r := httptest.NewRequest("POST", "http://localhost/", nil)
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Add(header, r.Header.Get(header))
		browserCoreCode(t, BrowserSessionOrigin(r, true), "forbidden")
	}
}

func TestBrowserSessionMutationAllowlistDoesNotGrowWithOtherHandlers(t *testing.T) {
	for _, test := range []struct {
		method, tail string
		allowed      bool
	}{
		{"POST", "messages", true}, {"POST", "messages/msg_123/seen", true}, {"POST", "messages/msg_123/ack", true}, {"PUT", "presentation", true},
		{"POST", "jobs", false}, {"PUT", "jobs/job_123", false}, {"POST", "workers", false},
		{"POST", "credentials", false}, {"POST", "publication", false}, {"POST", "review-source/refresh", false},
		{"POST", "messages/msg_123/delete", false}, {"DELETE", "messages/msg_123", false},
		{"POST", "messages/msg_123/ack/extra", false}, {"PUT", "presentation/history", false},
		{"PATCH", "presentation", false}, {"POST", "browser-session", false},
	} {
		r := httptest.NewRequest(test.method, "http://localhost/api/v1/projects/project/"+test.tail, nil)
		if browserMutationAllowed(r, "project") != test.allowed || browserMutationAllowed(r, "other") {
			t.Fatalf("unexpected cookie route admission: %s %s", test.method, test.tail)
		}
	}
}
