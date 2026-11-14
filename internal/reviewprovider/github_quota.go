package reviewprovider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type githubCallerKey struct{}

func WithGitHubCaller(ctx context.Context, caller string) context.Context {
	switch caller {
	case "review_selection", "cli_preparation", "hosted_preparation", "hosted_recovery", "lifecycle_metadata", "lifecycle_enrichment", "lifecycle_enrichment_hint", "lifecycle_enrichment_comparison", "lifecycle_enrichment_periodic", "ci_watch", "source_refresh", "history_import":
		return context.WithValue(ctx, githubCallerKey{}, caller)
	default:
		return ctx
	}
}

type githubQuotaKey struct{ caller, endpoint, resource string }

type githubQuotaCount struct {
	requests, conditional, notModified, gated, primary, otherLimited int
	remaining, reset, retryAfter                                     int64
}

// GitHubQuota aggregates requests for one configured credential. Bucket IDs
// are random for this process, so logs carry no credential-derived material.
type GitHubQuota struct {
	mu        sync.Mutex
	bucket    string
	next      time.Time
	rows      map[githubQuotaKey]githubQuotaCount
	coreFloor RetryTimingEvidence
}

func NewGitHubQuota() *GitHubQuota {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil
	}
	return &GitHubQuota{bucket: hex.EncodeToString(id[:]), rows: make(map[githubQuotaKey]githubQuotaCount)}
}

func (g *GitHub) SetQuotaTelemetry(quota *GitHubQuota) { g.quota = quota }

// primaryReadGate is a negative, process-local floor shared by adapters using
// the same credential. It neither proves availability nor changes admission.
// GraphQL and other quota resources keep their independent existing behavior.
func (q *GitHubQuota) primaryReadGate(now time.Time, method, path string) error {
	if q == nil || method != http.MethodGet || !(path == "/user" || strings.HasPrefix(path, "/repositories/") || strings.HasPrefix(path, "/repos/")) {
		return nil
	}
	q.mu.Lock()
	floor := q.coreFloor
	q.mu.Unlock()
	if floor.State != "proven" || !now.Before(floor.NotBefore) {
		return nil
	}
	seconds := int64((floor.NotBefore.Sub(now) + time.Second - 1) / time.Second)
	return &Error{Code: "provider_rate_limited", Message: "GitHub core reads are waiting for an observed primary reset; availability is not established.", Status: http.StatusTooManyRequests, RetryAfterSeconds: seconds, RetryTiming: floor, retryDirected: true}
}

func githubEndpoint(path string) string {
	if path == "/graphql" {
		return "pull_graphql"
	}
	if strings.HasPrefix(path, "/repositories/") || path == "/user" {
		return "identity"
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" {
		return "other"
	}
	switch parts[3] {
	case "actions":
		return "actions"
	case "branches", "rules":
		return "branch_rules"
	case "commits":
		return "commit_checks"
	case "issues":
		return "issue_comments"
	case "pulls":
		if len(parts) > 5 {
			switch parts[5] {
			case "comments", "reviews":
				return "pull_reviews"
			}
		}
		return "pull_metadata"
	default:
		return "other"
	}
}

func quotaNumber(header http.Header, name string) int64 {
	values := header.Values(name)
	if len(values) == 1 {
		if number, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64); err == nil && number >= 0 {
			return number
		}
	}
	return -1
}

func (q *GitHubQuota) observe(ctx context.Context, now time.Time, path string, response *http.Response, conditional, gated bool) {
	if q == nil {
		return
	}
	caller, _ := ctx.Value(githubCallerKey{}).(string)
	if caller == "" {
		caller = "other"
	}
	key := githubQuotaKey{caller, githubEndpoint(path), "unknown"}
	var header http.Header
	var status int
	if response != nil {
		header, status = response.Header, response.StatusCode
		switch resource := header.Get("X-RateLimit-Resource"); resource {
		case "core", "graphql", "search", "code_search", "integration_manifest", "dependency_snapshots", "code_scanning_upload", "actions_runner_registration", "scim", "audit_log":
			key.resource = resource
		}
	}
	remaining := quotaNumber(header, "X-RateLimit-Remaining")
	reset := quotaNumber(header, "X-RateLimit-Reset")
	retryAfter := quotaNumber(header, "Retry-After")
	if retryAfter < 0 {
		if date, err := http.ParseTime(header.Get("Retry-After")); err == nil {
			retryAfter = max(0, int64(date.Sub(now).Seconds()))
		}
	}
	var previous map[githubQuotaKey]githubQuotaCount
	q.mu.Lock()
	// Only an unambiguous primary-core refusal establishes this floor. A
	// successful concurrent request must not erase an unexpired refusal.
	if (status == http.StatusForbidden || status == http.StatusTooManyRequests) && remaining == 0 && len(header.Values("X-RateLimit-Resource")) == 1 && header.Get("X-RateLimit-Resource") == "core" {
		timing := sourceRetryTiming(header, now)
		if timing.State == "proven" && timing.NotBefore.After(now) && timing.NotBefore.After(q.coreFloor.NotBefore) {
			q.coreFloor = timing
		}
	}
	previousEnd := q.next
	if q.next.IsZero() || !now.Before(q.next) {
		previous, q.rows = q.rows, make(map[githubQuotaKey]githubQuotaCount)
		q.next = now.Add(time.Minute)
	}
	row, found := q.rows[key]
	if !found {
		row.remaining, row.reset, row.retryAfter = -1, -1, -1
	}
	if gated {
		row.gated++
	} else {
		row.requests++
	}
	if conditional {
		row.conditional++
	}
	if status == http.StatusNotModified && conditional {
		row.notModified++
	}
	if remaining >= 0 {
		row.remaining = remaining
	}
	if reset >= 0 {
		row.reset = reset
	}
	if retryAfter >= 0 {
		row.retryAfter = retryAfter
	}
	limited := status == http.StatusTooManyRequests || status == http.StatusForbidden && (remaining == 0 || header.Get("Retry-After") != "")
	firstLimit := limited && row.primary+row.otherLimited == 0
	if limited {
		if remaining == 0 {
			row.primary++
		} else {
			row.otherLimited++
		}
	}
	q.rows[key] = row
	q.mu.Unlock()
	if firstLimit {
		slog.WarnContext(ctx, "GitHub request rate limited", "bucket", q.bucket, "caller", key.caller, "endpoint", key.endpoint,
			"resource", key.resource, "http_status", status, "primary_exhausted", remaining == 0,
			"remaining", remaining, "reset_epoch", reset, "retry_after_seconds", retryAfter)
	}
	for key, row := range previous {
		slog.InfoContext(ctx, "GitHub request totals", "bucket", q.bucket, "window_ended", previousEnd, "caller", key.caller, "endpoint", key.endpoint, "resource", key.resource,
			"requests", row.requests, "conditional", row.conditional, "not_modified", row.notModified, "local_gate_skips", row.gated,
			"primary_limits", row.primary, "other_limits", row.otherLimited, "remaining", row.remaining, "reset_epoch", row.reset, "retry_after_seconds", row.retryAfter)
	}
}
