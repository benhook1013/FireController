package reviewprovider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type completionBrokenBody struct{}

func (completionBrokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (completionBrokenBody) Close() error             { return nil }

func TestReadCompletionRetainsBodyFailureAfterSuccessfulHeaders(t *testing.T) {
	q := NewGitHubQuota()
	calls := 0
	g := &GitHub{token: "synthetic", baseURL: githubAPIURL, quota: q, httpClient: &http.Client{Transport: ciRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{`"synthetic"`}}, Body: completionBrokenBody{}, Request: r}, nil
	})}}
	_, _, _, err := g.requestBytes(WithGitHubCaller(t.Context(), "lifecycle_enrichment_periodic"), http.MethodGet, "/repos/PRIVATE/repo/commits/PRIVATE/check-runs", nil, nil)
	if err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
	key := githubCompletionKey{"lifecycle_enrichment_periodic", "check_inventory"}
	got := q.completions[key]
	if got.Completed != 1 || got.ReadFailed != 1 || got.BodyFailed != 1 || got.TimedOut != 0 {
		t.Fatal(got)
	}
	for _, row := range q.rows {
		for _, d := range row.details {
			if d.NoResponse != 0 || d.Requests != 1 {
				t.Fatal("header accounting changed", d)
			}
		}
	}
	if len(g.cache) != 0 {
		t.Fatal("partial body cached")
	}
}

func TestReadCompletionClassifiesOnlySafeCountersAndBoundsWindow(t *testing.T) {
	q := NewGitHubQuota()
	ctx := WithGitHubCaller(t.Context(), "ci_watch")
	now := time.Now()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	cases := []struct {
		ctx   context.Context
		phase string
		err   error
	}{
		{ctx, "body", providerTransportError(ctx, context.DeadlineExceeded, false)},
		{canceled, "headers", providerTransportError(canceled, context.Canceled, false)},
		{ctx, "body", providerTransportError(ctx, errors.New("PRIVATE_BODY_URL"), false)},
		{ctx, "body", nil},
	}
	for _, c := range cases {
		q.observeReadCompletion(c.ctx, now, "/repos/PRIVATE/repo/actions/runs/123/attempts/2/jobs", now.Add(-25*time.Millisecond), c.phase, c.err)
	}
	got := q.completions[githubCompletionKey{"ci_watch", "actions_attempt_jobs"}]
	if got.Completed != 4 || got.ReadFailed != 3 || got.TimedOut != 1 || got.Canceled != 1 || got.BodyFailed != 2 || got.DurationMillis != 100 || got.MaxDurationMillis != 25 {
		t.Fatal(got)
	}
	for k := range q.completions {
		if strings.Contains(k.endpoint, "PRIVATE") {
			t.Fatal("selector leaked")
		}
	}
	q.observeReadCompletion(ctx, now.Add(time.Minute), "/repos/PRIVATE/repo/actions/runs/123", now.Add(time.Minute), "headers", nil)
	if len(q.completions) != 1 || q.completions[githubCompletionKey{"ci_watch", "actions_current_run"}].Completed != 1 {
		t.Fatal(q.completions)
	}
}

func TestReadCompletionDeadlineOriginUsesCapturedFailureIdentity(t *testing.T) {
	q := NewGitHubQuota()
	now := time.Now()
	caller, cancel := context.WithDeadline(t.Context(), now.Add(-time.Second))
	defer cancel()
	transportCtx, transportCancel := context.WithCancel(t.Context())
	transport := providerTransportError(transportCtx, context.DeadlineExceeded, false)
	// Cancellation after the failure must not rewrite its recorded origin.
	transportCancel()
	cases := []struct {
		name              string
		failure           error
		caller, transport int
	}{
		{"caller", providerTransportError(caller, context.DeadlineExceeded, false), 1, 0},
		{"transport", transport, 0, 1},
		{"unclassified", providerError("provider_unavailable", "safe", http.StatusRequestTimeout, 0, false), 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q = NewGitHubQuota()
			q.observeReadCompletion(t.Context(), now, "/PRIVATE", now, "headers", tc.failure)
			got := q.completions[githubCompletionKey{"other", "other"}]
			if got.Completed != 1 || got.ReadFailed != 1 || got.TimedOut != 1 || got.CallerDeadline != tc.caller || got.TransportTimeout != tc.transport || got.BodyFailed != 0 || got.Canceled != 0 {
				t.Fatal(got)
			}
		})
	}
}
