package reviewprovider

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// suiteCountPage is deliberately separate from pullReviewPage: existing pull
// review readers keep their current behavior. Header accounting happens once
// per physical request; completion describes transport/body work, not semantic
// GraphQL success. No body or query is logged. The caller classifies HTTP 200
// GraphQL errors only after strict validation.
func (g *GitHub) suiteCountPage(ctx context.Context, body []byte) (_ []byte, _ http.Header, _ int, resultErr error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, nil, 0, providerTransportError(ctx, nil, false)
	}
	if g.readGate != nil {
		if err := g.readGate(ctx); err != nil {
			g.quota.observe(ctx, time.Now(), "/graphql", nil, false, true)
			return nil, nil, 0, err
		}
	}
	if err := g.quota.suiteGraphQLReadGate(time.Now()); err != nil {
		g.quota.observe(ctx, time.Now(), "/graphql", nil, false, true)
		return nil, nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.baseURL, "/")+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, providerError("provider_unavailable", "The provider request could not be prepared.", 0, 0, false)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "FireController")
	if g.token != "" {
		request.Header.Set("Authorization", "Bearer "+g.token)
	}
	started := time.Now()
	phase := "headers"
	defer func() { g.quota.observeReadCompletion(ctx, time.Now(), "/graphql", started, phase, resultErr) }()
	response, err := g.httpClient.Do(request)
	g.quota.observe(ctx, time.Now(), "/graphql", response, false, false)
	if err != nil {
		return nil, nil, 0, providerTransportError(ctx, err, false)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
			if refusal := ciSuiteBatchPrimaryLimit(response.Header); refusal != nil {
				g.quota.observeSuiteGraphQLPrimary(ctx, response.StatusCode, refusal.RetryTiming)
			}
		}
		return nil, response.Header.Clone(), response.StatusCode, nil
	}
	phase = "body"
	data, err := io.ReadAll(io.LimitReader(response.Body, githubMaxBody+1))
	if len(data) > githubMaxBody {
		return nil, nil, 0, invalidResponse(false, response.StatusCode)
	}
	if err != nil {
		return nil, nil, 0, providerTransportError(ctx, err, false)
	}
	return data, response.Header.Clone(), response.StatusCode, nil
}

// Only this experimental suite transport consults the new floor. Core REST
// and legacy pull-review GraphQL retain their separate behavior. The common
// engine readGate above remains in force; this gate does not bypass it.
func (q *GitHubQuota) suiteGraphQLReadGate(now time.Time) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	floor := q.suiteGraphQLFloor
	q.mu.Unlock()
	if floor.State != "proven" || !now.Before(floor.NotBefore) {
		return nil
	}
	local := floor
	local.ObservedAt = now.UTC()
	seconds := int64((floor.NotBefore.Sub(now) + time.Second - 1) / time.Second)
	return &Error{Code: "provider_rate_limited", Message: "Local observation of a retained GraphQL suite-count reset floor; no provider request or availability is established.", Status: http.StatusTooManyRequests, RetryAfterSeconds: seconds, RetryTiming: local, retryDirected: true}
}

// This is an outcome-only hook: it never adds requests, target observations or
// completion records. HTTP 403/429 limits already appear in header counters.
// The separate cumulative HTTP-200 counter counts only validated body refusals
// absent from those header counters. Never sum it with all HTTP-200 requests.
func (q *GitHubQuota) observeSuiteGraphQLPrimary(ctx context.Context, status int, timing RetryTimingEvidence) {
	if q == nil || timing.State != "proven" || (status != http.StatusOK && status != http.StatusForbidden && status != http.StatusTooManyRequests) {
		return
	}
	now := time.Now()
	q.mu.Lock()
	if timing.NotBefore.After(now) && timing.NotBefore.After(q.suiteGraphQLFloor.NotBefore) {
		q.suiteGraphQLFloor = timing
	}
	if status == http.StatusOK {
		q.suiteGraphQLPrimary200++
	}
	count := q.suiteGraphQLPrimary200
	q.mu.Unlock()
	if status == http.StatusOK {
		caller, _ := ctx.Value(githubCallerKey{}).(string)
		if caller == "" {
			caller = "other"
		}
		slog.WarnContext(ctx, "GitHub suite GraphQL primary refusal", "bucket", q.bucket, "caller", caller, "query_kind", "suite_count", "http_status", status, "primary_200_total", count, "not_before", timing.NotBefore)
	}
}
