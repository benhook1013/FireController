package reviewprovider

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGitHubQuotaReportsRepeatedReadsAndSanitizedLimit(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	q := NewGitHubQuota()
	now := time.Now()
	ctx := WithGitHubCaller(t.Context(), "ci_watch")
	const endpoint = "/repos/private-owner/private-project/actions/runs"
	headers := http.Header{"X-Ratelimit-Resource": {"core"}, "X-Ratelimit-Remaining": {"4"}, "X-Ratelimit-Reset": {"2000000000"}}
	for range 3 {
		q.observe(ctx, now, endpoint, &http.Response{Header: headers, StatusCode: 304}, true, false)
	}
	headers.Set("X-RateLimit-Remaining", "0")
	headers.Set("Retry-After", "60")
	headers.Set("Private-Value", "private-provider-text")
	q.observe(ctx, now, endpoint, &http.Response{Header: headers, StatusCode: 429}, false, false)
	q.observe(ctx, now, endpoint, &http.Response{Header: headers, StatusCode: 429}, false, false)
	q.observe(ctx, now, endpoint, nil, false, true)
	q.observe(ctx, now.Add(time.Minute), "/user", nil, false, false)

	got := output.String()
	for _, want := range []string{`"caller":"ci_watch"`, `"endpoint":"actions"`, `"resource":"core"`, `"requests":5`, `"not_modified":3`, `"primary_limits":2`, `"local_gate_skips":1`, `"remaining":0`, `"retry_after_seconds":60`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in aggregate", want)
		}
	}
	if strings.Count(got, "GitHub request rate limited") != 1 || strings.Contains(got, "private-") {
		t.Fatal("limit log was duplicated or leaked provider identity")
	}
}
