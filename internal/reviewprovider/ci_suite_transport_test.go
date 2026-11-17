package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func suiteTelemetryTotals(q *GitHubQuota) (requests, gated, primary, completed, failed, bodyfailed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.rows {
		requests += r.requests
		gated += r.gated
		primary += r.primary
	}
	for _, r := range q.completions {
		completed += r.Completed
		failed += r.ReadFailed
		bodyfailed += r.BodyFailed
	}
	return
}
func TestSuiteTransportPrimaryFloorIsolation(t *testing.T) {
	for _, status := range []int{200, 403, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			reset := time.Now().Add(time.Hour).Truncate(time.Second)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("X-RateLimit-Resource", "graphql")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "private failure"}}})
			}))
			defer server.Close()
			q := NewGitHubQuota()
			a, b := testGitHub(server, ""), testGitHub(server, "")
			a.SetQuotaTelemetry(q)
			b.SetQuotaTelemetry(q)
			out := a.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
			var e *Error
			if !errors.As(out[0].Err, &e) || e.Status != status || e.Code != "provider_rate_limited" {
				t.Fatal(out)
			}
			floor := q.suiteGraphQLFloor
			out = b.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
			if calls != 1 || out[0].Err == nil || !q.suiteGraphQLFloor.NotBefore.Equal(floor.NotBefore) || !q.coreFloor.NotBefore.IsZero() {
				t.Fatal(calls, out, q)
			}
			requests, gated, primary, completed, failed, _ := suiteTelemetryTotals(q)
			expectedPrimary := 1
			expected200 := uint64(0)
			if status == 200 {
				expectedPrimary = 0
				expected200 = 1
			}
			if requests != 1 || gated != 1 || primary != expectedPrimary || completed != 1 || failed != 0 || q.suiteGraphQLPrimary200 != expected200 {
				t.Fatal(requests, gated, primary, completed, failed, q.suiteGraphQLPrimary200)
			}
			if q.primaryReadGate(time.Now(), "GET", "/repos/owner/repo/pulls/1") != nil {
				t.Fatal("core poisoned")
			}
			// Legacy GraphQL does not consult the suite floor and retains its behavior.
			if _, _, _, err := b.pullReviewPage(t.Context(), []byte(`{}`)); err != nil || calls != 2 {
				t.Fatal("legacy behavior changed", err, calls)
			}
			independent := testGitHub(server, "")
			independent.SetQuotaTelemetry(NewGitHubQuota())
			independent.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
			if calls != 3 {
				t.Fatal("credential isolation")
			}
			if q.suiteGraphQLReadGate(floor.NotBefore) != nil {
				t.Fatal("floor did not expire")
			}
			q.mu.Lock()
			q.suiteGraphQLFloor.NotBefore = time.Now().Add(-time.Second)
			q.mu.Unlock()
			b.suiteCountPage(t.Context(), []byte(`{}`))
			if calls != 4 {
				t.Fatal("expired floor blocked")
			}
		})
	}
}
func TestSuiteTransportNoFalseFloor(t *testing.T) {
	for _, mode := range []string{"last_point", "missing_resource", "duplicate_remaining", "bad_reset", "wrong_resource", "alias_error"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h := w.Header()
				h.Set("X-RateLimit-Resource", "graphql")
				h.Set("X-RateLimit-Remaining", "0")
				h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
				switch mode {
				case "missing_resource":
					h.Del("X-RateLimit-Resource")
				case "duplicate_remaining":
					h.Add("X-RateLimit-Remaining", "1")
				case "bad_reset":
					h.Set("X-RateLimit-Reset", "bad")
				case "wrong_resource":
					h.Set("X-RateLimit-Resource", "core")
				case "alias_error":
					h.Set("X-RateLimit-Remaining", "10")
				}
				env := suiteBatchEnvelope(suiteBatchNode(ciHead, 1), suiteBatchNode(ciHead, 1))
				if mode != "last_point" {
					env["errors"] = []any{map[string]any{"message": "private"}}
				}
				json.NewEncoder(w).Encode(env)
			}))
			defer server.Close()
			g := testGitHub(server, "")
			q := NewGitHubQuota()
			g.SetQuotaTelemetry(q)
			out := g.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
			if (out[0].Err == nil) != (mode == "last_point") || !q.suiteGraphQLFloor.NotBefore.IsZero() || q.suiteGraphQLPrimary200 != 0 {
				t.Fatal(out, q)
			}
		})
	}
}

type suiteRoundTripper func(*http.Request) (*http.Response, error)

func (f suiteRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type suiteFailedBody struct{ err error }

func (b suiteFailedBody) Read([]byte) (int, error) { return 0, b.err }
func (b suiteFailedBody) Close() error             { return nil }
func TestSuiteTransportBodyCompletion(t *testing.T) {
	for _, mode := range []string{"broken", "cancel", "deadline", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var deadlineCancel context.CancelFunc
			if mode == "deadline" {
				ctx, deadlineCancel = context.WithTimeout(t.Context(), time.Millisecond)
				defer deadlineCancel()
			}
			g := &GitHub{baseURL: "https://example.test", quota: NewGitHubQuota(), httpClient: &http.Client{Transport: suiteRoundTripper(func(r *http.Request) (*http.Response, error) {
				var body io.ReadCloser = suiteFailedBody{io.ErrUnexpectedEOF}
				if mode == "cancel" {
					cancel()
					body = suiteFailedBody{context.Canceled}
				}
				if mode == "deadline" {
					<-r.Context().Done()
					body = suiteFailedBody{context.DeadlineExceeded}
				}
				if mode == "oversize" {
					body = io.NopCloser(strings.NewReader(strings.Repeat("x", githubMaxBody+1)))
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body, Request: r}, nil
			})}}
			_, _, _, err := g.suiteCountPage(ctx, []byte(`{}`))
			if err == nil {
				t.Fatal("missing failure")
			}
			req, gate, _, completed, failed, bodyfailed := suiteTelemetryTotals(g.quota)
			if req != 1 || gate != 0 || completed != 1 || failed != 1 || bodyfailed != map[bool]int{true: 0, false: 1}[mode == "oversize"] {
				t.Fatal(req, gate, completed, failed, bodyfailed)
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatal(err)
			}
			if mode == "cancel" && !errors.Is(e.cause, context.Canceled) {
				t.Fatal(e)
			}
			if mode == "deadline" && (e.Status != 408 || !errors.Is(e.cause, context.DeadlineExceeded)) {
				t.Fatal(e)
			}
		})
	}
}
func TestSuiteTransportExternalGateAndPrecancel(t *testing.T) {
	calls := 0
	g := &GitHub{baseURL: "https://example.test", quota: NewGitHubQuota(), httpClient: &http.Client{Transport: suiteRoundTripper(func(r *http.Request) (*http.Response, error) { calls++; return nil, io.ErrUnexpectedEOF })}}
	expected := providerError("provider_rate_limited", "existing gate", 429, 60, false)
	g.SetReadGate(func(context.Context) error { return expected })
	_, _, _, err := g.suiteCountPage(t.Context(), nil)
	if err != expected {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	g.suiteCountPage(ctx, nil)
	req, gates, _, done, _, _ := suiteTelemetryTotals(g.quota)
	if calls != 0 || req != 0 || gates != 1 || done != 0 {
		t.Fatal(calls, req, gates, done)
	}
}
func TestSuiteGraphQLFloorConcurrentNonRegression(t *testing.T) {
	q := NewGitHubQuota()
	now := time.Now()
	long := RetryTimingEvidence{State: "proven", ObservedAt: now, NotBefore: now.Add(time.Hour)}
	short := long
	short.NotBefore = now.Add(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.observeSuiteGraphQLPrimary(t.Context(), 403, long)
			q.observeSuiteGraphQLPrimary(t.Context(), 200, short)
			q.suiteGraphQLReadGate(now)
		}()
	}
	wg.Wait()
	if !q.suiteGraphQLFloor.NotBefore.Equal(long.NotBefore) || q.suiteGraphQLPrimary200 != 20 {
		t.Fatal(q)
	}
}
