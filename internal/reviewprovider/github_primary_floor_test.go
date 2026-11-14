package reviewprovider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func primaryHeaders(now time.Time) http.Header {
	return http.Header{"X-Ratelimit-Resource": {"core"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(time.Minute).Unix(), 10)}}
}

func TestPrimaryFloorSharedAcrossCallersWithoutHTTP(t *testing.T) {
	var calls atomic.Int64
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		for k, v := range primaryHeaders(now) {
			w.Header()[k] = v
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	quota := NewGitHubQuota()
	observer, selection := testGitHub(server, "same"), testGitHub(server, "same")
	observer.SetQuotaTelemetry(quota)
	selection.SetQuotaTelemetry(quota)
	_, _, status, err := observer.requestBytes(WithGitHubCaller(t.Context(), "lifecycle_enrichment"), http.MethodGet, "/repos/a/b/actions/runs", url.Values{}, nil)
	if err != nil || status != 403 {
		t.Fatal("initial refusal not observed", status, err)
	}
	for _, caller := range []string{"review_selection", "cli_preparation", "hosted_preparation", "hosted_recovery", "source_refresh", "ci_watch", "lifecycle_metadata"} {
		_, _, _, err = selection.requestBytes(WithGitHubCaller(t.Context(), caller), http.MethodGet, "/repositories/1", nil, nil)
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "provider_rate_limited" || failure.RetryTiming.State != "proven" || failure.RetryAfterSeconds <= 0 {
			t.Fatalf("%s missing proven local refusal: %v", caller, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("exhausted credential sent extra requests", calls.Load())
	}
	independent := testGitHub(server, "other")
	independent.SetQuotaTelemetry(NewGitHubQuota())
	_, _, _, _ = independent.requestBytes(t.Context(), http.MethodGet, "/repositories/1", nil, nil)
	if calls.Load() != 2 {
		t.Fatal("credential isolation lost")
	}
}

func TestPrimaryFloorBoundariesAndConcurrentSuccess(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	q := NewGitHubQuota()
	q.observe(t.Context(), now, "/repos/a/b/actions/runs", &http.Response{StatusCode: 403, Header: primaryHeaders(now)}, false, false)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.observe(t.Context(), now.Add(time.Second), "/repos/a/b/pulls/1", &http.Response{StatusCode: 200, Header: http.Header{"X-Ratelimit-Remaining": {"200"}}}, false, false)
			if q.primaryReadGate(now.Add(time.Second), http.MethodGet, "/repositories/1") == nil {
				t.Error("success erased floor")
			}
		}()
	}
	wg.Wait()
	if q.primaryReadGate(now.Add(time.Minute+5*time.Second-time.Nanosecond), http.MethodGet, "/user") == nil {
		t.Fatal("early reset")
	}
	if q.primaryReadGate(now.Add(time.Minute+5*time.Second), http.MethodGet, "/user") != nil {
		t.Fatal("expired floor did not permit ordinary retry")
	}
	for _, target := range []struct{ method, path string }{{http.MethodPost, "/graphql"}, {http.MethodGet, "/search/issues"}, {http.MethodPost, "/repos/a/b/issues/1/comments"}} {
		if q.primaryReadGate(now, target.method, target.path) != nil {
			t.Fatal("core read floor affected other resource/mutation", target)
		}
	}
	// A shorter refusal cannot reduce a later existing floor.
	h := primaryHeaders(now)
	h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(10*time.Second).Unix(), 10))
	q.observe(t.Context(), now, "/user", &http.Response{StatusCode: 403, Header: h}, false, false)
	if q.primaryReadGate(now.Add(20*time.Second), http.MethodGet, "/user") == nil {
		t.Fatal("later floor shortened")
	}
}

func TestPrimaryFloorRefusesAmbiguousOrNonPrimaryEvidence(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	for _, name := range []string{"secondary", "missing_resource", "duplicate_resource", "duplicate_reset", "invalid_reset", "expired_reset", "too_far", "success", "graphql"} {
		t.Run(name, func(t *testing.T) {
			q := NewGitHubQuota()
			h := primaryHeaders(now)
			status := 403
			switch name {
			case "secondary":
				h.Set("X-RateLimit-Remaining", "12")
				h.Set("Retry-After", "60")
			case "missing_resource":
				h.Del("X-RateLimit-Resource")
			case "duplicate_resource":
				h.Add("X-RateLimit-Resource", "core")
			case "duplicate_reset":
				h.Add("X-RateLimit-Reset", h.Get("X-RateLimit-Reset"))
			case "invalid_reset":
				h.Set("X-RateLimit-Reset", "unknown")
			case "expired_reset":
				h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(-10*time.Second).Unix(), 10))
			case "too_far":
				h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10))
			case "success":
				status = 200
			case "graphql":
				h.Set("X-RateLimit-Resource", "graphql")
			}
			q.observe(t.Context(), now, "/user", &http.Response{StatusCode: status, Header: h}, false, false)
			if q.primaryReadGate(now, http.MethodGet, "/user") != nil {
				t.Fatal("unproven/non-core evidence became floor")
			}
		})
	}
}

func TestPrimaryFloorLocalObservationPreservesOriginalProviderProvenance(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	q := NewGitHubQuota()
	q.observe(t.Context(), now, "/user", &http.Response{StatusCode: 403, Header: primaryHeaders(now)}, false, false)
	later := now.Add(20 * time.Second)
	err := q.primaryReadGate(later, http.MethodGet, "/user")
	var failure *Error
	if !errors.As(err, &failure) || !failure.RetryTiming.ObservedAt.Equal(later) || !failure.RetryTiming.NotBefore.Equal(now.Add(65*time.Second)) {
		t.Fatal("local observation changed absolute floor or retained stale attempt timestamp", err)
	}
	q.mu.Lock()
	original := q.coreFloor
	q.mu.Unlock()
	if !original.ObservedAt.Equal(now) || !original.NotBefore.Equal(failure.RetryTiming.NotBefore) {
		t.Fatal("provider provenance was rewritten")
	}
}
