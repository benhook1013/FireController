package reviewprovider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// MetadataPairQualification reports compatibility of one bounded observation,
// never metadata facts, source-publication authority, or credential identities.
type MetadataPairQualification struct {
	Bucket   string `json:"bucket,omitempty"`
	State    string `json:"state"`
	Requests int64  `json:"requests"`
}

type metadataQualificationTransport struct {
	base         http.RoundTripper
	host, scheme string
	pullPaths    [2]string
	calls        atomic.Int64
}

func (t *metadataQualificationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	allowed := r.Method == http.MethodPost && r.URL.Path == "/graphql"
	for _, path := range t.pullPaths {
		if r.Method == http.MethodGet && r.URL.Path == path {
			allowed = true
		}
	}
	if !allowed || r.URL.Host != t.host || r.URL.Scheme != t.scheme || r.URL.User != nil || r.URL.Fragment != "" || r.URL.RawQuery != "" || r.URL.Opaque != "" || r.URL.RawPath != "" {
		return nil, errors.New("metadata qualification destination refused")
	}
	for {
		n := t.calls.Load()
		if n >= 5 {
			return nil, errors.New("metadata qualification request budget exhausted")
		}
		if t.calls.CompareAndSwap(n, n+1) {
			break
		}
	}
	return t.base.RoundTrip(r)
}

// QualifyMetadataPair brackets a single paired query with ordinary REST reads.
// The caller must hold its existing account/project reservation and apply any
// provider error to normal cooldown. No diagnostic facts may be published as an
// ordinary observation. "matched" proves only this non-atomic bounded sample.
func (g *GitHub) QualifyMetadataPair(ctx context.Context, id int64, repository string, numbers [2]int64) (result MetadataPairQualification, err error) {
	result.State = "refused"
	if ctx == nil || g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || id <= 0 || !validRepositorySelector(repository) || !validPullNumber(numbers[0]) || !validPullNumber(numbers[1]) || numbers[0] == numbers[1] || numbers[0] > 2147483647 || numbers[1] > 2147483647 {
		return result, invalidSelector()
	}
	if g.quota != nil {
		result.Bucket = g.quota.bucket
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Shared diagnostics do not attribute either member's work to the first PR.
	ctx = context.WithValue(ctx, githubTargetContextKey{}, githubTargetKey{})
	endpoint, _ := url.Parse(g.baseURL)
	transport := g.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	bounded := &metadataQualificationTransport{base: transport, host: endpoint.Host, scheme: endpoint.Scheme}
	for i, number := range numbers {
		bounded.pullPaths[i] = "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10)
	}
	client := *g.httpClient
	client.Transport = bounded
	// A private adapter view avoids copying mutexes or sharing conditional caches.
	// Credentials, read admission, client policy and the quota ledger stay actual.
	probe := &GitHub{readGate: g.readGate, quota: g.quota, token: g.token, baseURL: g.baseURL, httpClient: &client}
	defer func() { result.Requests = bounded.calls.Load() }()
	read := func() (out [2]PullMetadataPairResult, err error) {
		for i, number := range numbers {
			out[i].Pull, out[i].Repository, err = probe.PullWithRepository(ctx, repository, number)
			if err != nil {
				return out, err
			}
			if out[i].Repository.ID != id || !strings.EqualFold(out[i].Repository.FullName, repository) {
				return out, invalidResponse(false, http.StatusOK)
			}
		}
		return out, nil
	}
	before, err := read()
	if err != nil {
		return result, err
	}
	paired := probe.PullMetadataPair(ctx, id, repository, numbers)
	for _, row := range paired {
		if row.Err != nil {
			return result, row.Err
		}
	}
	after, err := read()
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, providerTransportError(ctx, ctx.Err(), false)
	}
	result.State = "inconclusive"
	// Compare complete normalized structs, including presence/absence of optional
	// values, so adding a future field cannot silently remove it from this proof.
	if reflect.DeepEqual(before, paired) && reflect.DeepEqual(before, after) {
		result.State = "matched"
	}
	return result, nil
}
