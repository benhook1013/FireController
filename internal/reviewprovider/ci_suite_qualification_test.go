package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSuiteQualificationBoundedComparison(t *testing.T) {
	for _, mode := range []string{"matched", "movement", "rate_limited", "partial", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			heads := [2]string{ciHead, strings.Repeat("d", 40)}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "rate_limited" && calls == 3 {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(429)
					return
				}
				if r.URL.Path == "/graphql" {
					envelope := suiteBatchEnvelope(suiteBatchNode(heads[0], 1), suiteBatchNode(heads[1], 1))
					data := envelope["data"].(map[string]any)
					data["rateLimit"] = map[string]any{"cost": 1}
					if mode == "partial" {
						data["repository"].(map[string]any)["c1"] = nil
					}
					json.NewEncoder(w).Encode(envelope)
					return
				}
				head := heads[0]
				if strings.Contains(r.URL.Path, heads[1]) {
					head = heads[1]
				}
				total := 1
				if mode == "movement" && calls > 3 {
					total = 2
				}
				json.NewEncoder(w).Encode(map[string]any{"total_count": total, "check_suites": []any{map[string]any{"id": 20, "head_sha": head, "app": map[string]any{"id": 10}}}})
			}))
			defer server.Close()
			ctx := t.Context()
			if mode == "cancelled" {
				var cancel func()
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := qualifyCISuiteBatch(ctx, testGitHub(server, ""), "owner/repo", heads)
			switch mode {
			case "matched":
				if err != nil || got.State != "matched" || calls != 5 || got.Requests != 5 {
					t.Fatalf("%+v calls=%d err=%v", got, calls, err)
				}
			case "movement":
				if err != nil || got.State != "inconclusive" || calls != 5 {
					t.Fatalf("%+v calls=%d err=%v", got, calls, err)
				}
			case "cancelled":
				if err == nil || calls != 0 || got.Requests != 0 {
					t.Fatalf("%+v calls=%d err=%v", got, calls, err)
				}
			default:
				if err == nil || calls != 3 || got.State != "refused" {
					t.Fatalf("%+v calls=%d err=%v", got, calls, err)
				}
			}
		})
	}
}

type qualificationTestRoundTripper func(*http.Request) (*http.Response, error)

func (f qualificationTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
func TestSuiteQualificationTransportBudgetAndDestination(t *testing.T) {
	var calls atomic.Int64
	bounded := &qualificationTransport{host: "api.github.com", scheme: "https", repository: "owner/repo", heads: [2]string{ciHead, strings.Repeat("d", 40)}, base: qualificationTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequest("POST", "https://api.github.com/graphql", nil)
			resp, _ := bounded.RoundTrip(r)
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 5 || bounded.calls.Load() != 5 {
		t.Fatalf("physical=%d bounded=%d", calls.Load(), bounded.calls.Load())
	}
	for _, destination := range []string{"https://other.example/graphql", "https://api.github.com/user", "http://api.github.com/graphql", "https://user:password@api.github.com/graphql"} {
		r, _ := http.NewRequest("POST", destination, nil)
		if _, err := bounded.RoundTrip(r); err == nil {
			t.Fatal("destination accepted")
		}
	}
	if calls.Load() != 5 {
		t.Fatal("refused destination reached transport")
	}
}

func TestSuiteQualificationPreservesLiveReadGateAndQuota(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected", 500) }))
	defer server.Close()
	g := testGitHub(server, "")
	q := NewGitHubQuota()
	g.quota = q
	gateCalls := 0
	g.readGate = func(context.Context) error { gateCalls++; return errors.New("read admission refused") }
	originalClient := g.httpClient
	got, err := qualifyCISuiteBatch(t.Context(), g, "owner/repo", [2]string{ciHead, strings.Repeat("d", 40)})
	if err == nil || calls != 0 || gateCalls != 1 || got.Requests != 0 || g.quota != q || got.Bucket != q.bucket || got.Bucket == "" || g.httpClient != originalClient {
		t.Fatalf("admission bypassed or live adapter mutated: %+v calls=%d gates=%d err=%v", got, calls, gateCalls, err)
	}
}

func TestSuiteQualificationBucketIsExistingProcessLabelNotCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unexpected", 500) }))
	defer server.Close()
	g := testGitHub(server, "PRIVATE_CREDENTIAL_SENTINEL")
	g.quota = NewGitHubQuota()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, _ := qualifyCISuiteBatch(ctx, g, "owner/repo", [2]string{ciHead, strings.Repeat("d", 40)})
	encoded, _ := json.Marshal(got)
	if got.Bucket != g.quota.bucket || len(got.Bucket) != 16 || strings.Contains(string(encoded), "PRIVATE_CREDENTIAL") || strings.Contains(string(encoded), "owner/repo") || strings.Contains(string(encoded), ciHead) {
		t.Fatalf("unsafe or missing correlation: %s", encoded)
	}
	other := NewGitHubQuota()
	if got.Bucket == other.bucket {
		t.Fatal("process labels unexpectedly reused")
	}
}
