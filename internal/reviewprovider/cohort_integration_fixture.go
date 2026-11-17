//go:build cohortintegration

package reviewprovider

import (
	"net/http"
	"net/url"
	"time"
)

// NewCohortIntegrationFixture is compiled only by the explicit synthetic test
// target. It cannot select a remote host or use a real credential. Normal
// production builds do not contain this transport seam.
func NewCohortIntegrationFixture(address string) (*GitHub, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalidSelector()
	}
	return &GitHub{token: "synthetic-cohort-integration", baseURL: address, httpClient: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
