package reviewprovider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// CISuiteQualification contains only bounded read evidence, never a credential,
// repository selector, provider body or a required-CI/merge verdict.
type CISuiteQualification struct {
	State    string   `json:"state"`
	Before   [2]int64 `json:"before"`
	Batched  [2]int64 `json:"batched"`
	After    [2]int64 `json:"after"`
	Requests int64    `json:"requests"`
}

// QualifySuiteBatch runs only inside an existing admitted cohort. It grants no
// CI authority and does not mutate the cohort consumption state. Callers must
// preserve the account reservation and feed provider failures into cooldown.
func (c *CIGateCohort) QualifySuiteBatch(ctx context.Context) (CISuiteQualification, error) {
	if c == nil || ctx == nil || !c.mu.TryLock() {
		return CISuiteQualification{State: "refused"}, invalidSelector()
	}
	defer c.mu.Unlock()
	if len(c.ordered) != 2 || c.batched != nil || c.initial != nil {
		return CISuiteQualification{State: "refused"}, invalidSelector()
	}
	for _, used := range c.members {
		if used {
			return CISuiteQualification{State: "refused"}, invalidSelector()
		}
	}
	deadline, _ := c.ctx.Deadline()
	bounded, cancel := context.WithDeadline(ctx, deadline)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	if c.ctx.Err() != nil {
		cancel()
	}
	// No first-member attribution for a shared diagnostic.
	bounded = context.WithValue(bounded, githubTargetContextKey{}, githubTargetKey{})
	return qualifyCISuiteBatch(bounded, c.g, c.ordered[0].Repository, [2]string{c.ordered[0].HeadSHA, c.ordered[1].HeadSHA})
}

type qualificationTransport struct {
	base                     http.RoundTripper
	host, scheme, repository string
	heads                    [2]string
	calls                    atomic.Int64
}

func (t *qualificationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	allowed := r.Method == http.MethodPost && r.URL.Path == "/graphql"
	for _, h := range t.heads {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/"+t.repository+"/commits/"+h+"/check-suites" {
			allowed = true
		}
	}
	if !allowed || r.URL.Host != t.host || r.URL.Scheme != t.scheme || r.URL.User != nil || r.URL.Fragment != "" {
		return nil, errors.New("qualification destination refused")
	}
	for {
		n := t.calls.Load()
		if n >= 5 {
			return nil, errors.New("qualification request budget exhausted")
		}
		if t.calls.CompareAndSwap(n, n+1) {
			break
		}
	}
	return t.base.RoundTrip(r)
}
func qualifyCISuiteBatch(ctx context.Context, g *GitHub, repository string, heads [2]string) (result CISuiteQualification, err error) {
	result.State = "refused"
	if ctx == nil || g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || !validRepositorySelector(repository) || !lowerCISHA(heads[0]) || !lowerCISHA(heads[1]) || heads[0] == heads[1] {
		return result, invalidSelector()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	endpoint, _ := url.Parse(g.baseURL)
	transport := g.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	bounded := &qualificationTransport{base: transport, host: endpoint.Host, scheme: endpoint.Scheme, repository: repository, heads: heads}
	client := *g.httpClient
	client.Transport = bounded
	// Construct a private view explicitly; never shallow-copy mutexes/caches or
	// replace the daemon adapter. Preserve the actual credential, shared quota
	// and read gate, while adding only this bounded transport wrapper.
	probe := &GitHub{readGate: g.readGate, quota: g.quota, token: g.token, baseURL: g.baseURL, httpClient: &client}
	defer func() { result.Requests = bounded.calls.Load() }()
	budgets := [2]*actionsBudget{{}, {}}
	for i, h := range heads {
		result.Before[i], err = probe.ciSuiteCount(ctx, "/repos/"+repository+"/commits/"+h+"/check-suites", h, budgets[i])
		if err != nil {
			return result, err
		}
	}
	rows := probe.ciSuiteCountsBatch(ctx, repository, heads, budgets)
	for i, row := range rows {
		if row.Err != nil {
			return result, row.Err
		}
		result.Batched[i] = row.Count
	}
	for i, h := range heads {
		result.After[i], err = probe.ciSuiteCount(ctx, "/repos/"+repository+"/commits/"+h+"/check-suites", h, budgets[i])
		if err != nil {
			return result, err
		}
	}
	result.State = "matched"
	if result.Before != result.Batched || result.Before != result.After {
		result.State = "inconclusive"
	}
	return result, nil
}
