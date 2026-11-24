package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPullMetadataPairOneBoundedQueryAndTelemetry(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Query     string
			Variables map[string]any
		}
		if r.Method != "POST" || r.URL.Path != "/graphql" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Query != metadataPairQuery || body.Variables["owner"] != "owner" || body.Variables["repo"] != "repo" || body.Variables["n0"] != float64(7) || body.Variables["n1"] != float64(8) {
			t.Error("wrong bounded request")
		}
		w.Header().Set("X-RateLimit-Resource", "graphql")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		json.NewEncoder(w).Encode(metadataPairFixture())
	}))
	defer server.Close()
	g := testGitHub(server, "synthetic")
	g.quota = NewGitHubQuota()
	got := g.PullMetadataPair(t.Context(), 20, "owner/repo", [2]int64{7, 8})
	if got[0].Err != nil || got[1].Err != nil || got[0].Pull.Number != 7 || got[1].Pull.Number != 8 || calls.Load() != 1 {
		t.Fatal(got, calls.Load())
	}
	g.quota.mu.Lock()
	defer g.quota.mu.Unlock()
	row := g.quota.rows[githubQuotaKey{"other", "pull_metadata_graphql", "graphql"}]
	if row.requests != 1 || row.suiteCostKnown != 1 || row.suitePointsSum != 1 || g.quota.completions[githubCompletionKey{"other", "pull_metadata_graphql"}].Completed != 1 {
		t.Fatal("wrong physical request/cost/completion accounting", row)
	}
	if _, exists := g.quota.rows[githubQuotaKey{"other", "suite_count_graphql", "graphql"}]; exists {
		t.Fatal("metadata mislabeled as suite count")
	}
}

func TestPullMetadataPairRefusalsAndNoAutomaticFallback(t *testing.T) {
	for _, mode := range []string{"gate", "canceled", "deadline", "429", "403", "primary200", "partial200", "oversized", "duplicate", "wrong_member"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("X-RateLimit-Resource", "graphql")
				switch mode {
				case "deadline":
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				case "429", "403":
					status, _ := strconv.Atoi(mode)
					w.Header().Set("Retry-After", "17")
					w.WriteHeader(status)
					return
				case "primary200":
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
					w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED"}]}`))
					return
				case "partial200":
					w.Write([]byte(`{"data":null,"errors":[{"type":"OTHER"}]}`))
					return
				case "oversized":
					w.Write([]byte(strings.Repeat("x", (64<<10)+1)))
					return
				}
				f := metadataPairFixture()
				if mode == "wrong_member" {
					f["data"].(map[string]any)["repository"].(map[string]any)["p0"].(map[string]any)["number"] = 99
				}
				b, _ := json.Marshal(f)
				if mode == "duplicate" {
					b = []byte(strings.Replace(string(b), `"p0":`, `"p0":null,"p0":`, 1))
				}
				w.Write(b)
			}))
			defer server.Close()
			defer close(release) // Release the fixture even if HTTP/1 body cancellation is not detected server-side.
			g := testGitHub(server, "synthetic")
			g.quota = NewGitHubQuota()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "deadline" {
				// Exercise a real deadline on the stalled response only. The
				// other cases need to test their response, independent of CI load.
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Second)
				defer stop()
			}
			if mode == "gate" {
				g.readGate = func(context.Context) error { return &Error{Code: "provider_rate_limited", RetryAfterSeconds: 17} }
			}
			if mode == "canceled" {
				cancel()
			}
			got := g.PullMetadataPair(ctx, 20, "owner/repo", [2]int64{7, 8})
			if got[0].Err == nil || (mode != "wrong_member" && got[1].Err == nil) || (mode == "wrong_member" && got[1].Err != nil) {
				t.Fatal(mode, got)
			}
			expected := int64(1)
			if mode == "gate" || mode == "canceled" {
				expected = 0
			}
			if calls.Load() != expected {
				t.Fatal("extra request/fallback", mode, calls.Load())
			}
			if mode == "primary200" {
				var e *Error
				if !errors.As(got[0].Err, &e) || e.Code != "provider_rate_limited" {
					t.Fatal("primary limit not retained", got[0].Err)
				}
				// Suite reads must honor the same credential's metadata reset floor.
				_, _, _, err := g.suiteCountPage(t.Context(), []byte(`{}`))
				if err == nil || calls.Load() != 1 {
					t.Fatal("shared reset floor bypassed", err, calls.Load())
				}
			}
		})
	}
}
func TestPullMetadataPairHonorsExistingSuiteFloor(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	g := testGitHub(server, "synthetic")
	g.quota = NewGitHubQuota()
	h := http.Header{}
	h.Set("X-RateLimit-Resource", "graphql")
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	failure := ciSuiteBatchPrimaryLimit(h)
	if failure == nil {
		t.Fatal("fixture")
	}
	g.quota.observeSuiteGraphQLPrimary(t.Context(), http.StatusOK, failure.RetryTiming)
	got := g.PullMetadataPair(t.Context(), 20, "owner/repo", [2]int64{7, 8})
	if got[0].Err == nil || got[1].Err == nil || calls.Load() != 0 {
		t.Fatal("metadata bypassed suite account floor", got, calls.Load())
	}
}
