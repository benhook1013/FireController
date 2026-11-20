package reviewprovider

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type ciLogRoundTrip func(*http.Request) (*http.Response, error)

func (f ciLogRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCIFailedTestNamesUsesOneCredentialFreeSignedDownload(t *testing.T) {
	const good = "https://productionresultssa32.blob.core.windows.net/logs/path?sig=private"
	for _, tc := range []struct {
		name, location string
		downloadStatus int
		want           []string
		requests       int
	}{
		{"exact signed response", good, 200, []string{"TestDoesNotSkip", "ExampleSpec.reportsTimeout"}, 2},
		{"no second redirect", good, 302, nil, 2},
		{"wrong origin", "https://foreign.invalid/logs?sig=private", 200, nil, 1},
		{"wrong port", "https://productionresultssa32.blob.core.windows.net:444/logs?sig=private", 200, nil, 1},
		{"oversize", good, 201, nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := &GitHub{baseURL: githubAPIURL, token: "test-token", httpClient: &http.Client{Transport: ciLogRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				out := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
				if calls == 1 {
					if r.URL.Host != githubAPIHost || r.URL.Path != "/repos/owner/repo/actions/jobs/123/logs" || r.Header.Get("Authorization") != "Bearer test-token" {
						t.Fatal("incorrect authenticated endpoint")
					}
					out.StatusCode = 302
					out.Header.Set("Location", tc.location)
					return out, nil
				}
				if r.URL.String() != good || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
					t.Fatal("credential or selector crossed redirect")
				}
				out.StatusCode = tc.downloadStatus
				if tc.downloadStatus == 302 {
					out.Header.Set("Location", "https://foreign.invalid/next")
				}
				if tc.downloadStatus == 201 {
					out.StatusCode = 200
					out.ContentLength = ciFailureLogLimit + 1
				}
				out.Body = io.NopCloser(strings.NewReader("2026-11-20T06:00:00.123456Z --- FAIL: TestDoesNotSkip/synthetic (0.01s)\n--- FAIL: TestDoesNotSkip (0.01s)\nExampleSpec > reportsTimeout() FAILED\n--- FAIL: TestWrong Has Spaces (0.01s)\nrandom non-test output\n"))
				return out, nil
			})}}
			got := g.CIFailedTestNames(t.Context(), "owner/repo", 123)
			if calls != tc.requests || !reflect.DeepEqual(got, tc.want) {
				t.Fatal(calls, got)
			}
		})
	}
}
