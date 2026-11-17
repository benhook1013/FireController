package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func qualificationMetadataREST(n int64) map[string]any {
	repo := func() map[string]any {
		return map[string]any{"id": 20, "full_name": "owner/repo", "default_branch": "main"}
	}
	return map[string]any{
		"title": "Metadata proof", "number": n, "state": "open", "merged": false,
		"head":          map[string]any{"sha": candidateHead, "ref": "feature", "repo": repo()},
		"base":          map[string]any{"sha": candidateBase, "ref": "main", "repo": repo()},
		"changed_files": 3, "additions": 4, "deletions": 2, "draft": false,
		"merged_at": nil, "mergeable": true, "mergeable_state": "clean",
	}
}

func TestMetadataQualificationCompleteNormalizedComparison(t *testing.T) {
	for _, mode := range []string{"matched", "title", "head", "head_ref", "base", "base_ref", "files", "additions", "deletions", "draft", "mergeability", "merge_state", "default_branch", "head_repository", "missing_optional", "after_movement"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer qualification-secret" {
					t.Error("qualification did not use actual adapter credential")
				}
				if r.URL.Path == "/graphql" {
					f := metadataPairFixture()
					p := f["data"].(map[string]any)["repository"].(map[string]any)["p0"].(map[string]any)
					switch mode {
					case "title":
						p["title"] = "Moved title"
					case "head":
						p["headRefOid"] = strings.Repeat("f", 40)
					case "head_ref":
						p["headRefName"] = "another"
					case "base":
						p["baseRefOid"] = strings.Repeat("e", 40)
					case "base_ref":
						p["baseRefName"] = "develop"
					case "files":
						p["changedFiles"] = 4
					case "additions":
						p["additions"] = 5
					case "deletions":
						p["deletions"] = 3
					case "draft":
						p["isDraft"] = true
					case "mergeability":
						p["mergeable"] = "UNKNOWN"
					case "merge_state":
						p["mergeStateStatus"] = "BLOCKED"
					case "default_branch":
						p["baseRepository"].(map[string]any)["defaultBranchRef"] = map[string]any{"name": "develop"}
					case "head_repository":
						p["headRepository"] = nil
					}
					json.NewEncoder(w).Encode(f)
					return
				}
				n, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/pulls/"), 10, 64)
				if err != nil {
					t.Error("unexpected REST endpoint")
				}
				p := qualificationMetadataREST(n)
				if mode == "missing_optional" {
					delete(p, "additions")
					delete(p, "deletions")
				}
				if mode == "after_movement" && call > 3 {
					p["title"] = "Moved after query"
				}
				json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			g := testGitHub(server, "qualification-secret")
			g.quota = NewGitHubQuota()
			original := g.httpClient
			var gates atomic.Int64
			g.readGate = func(ctx context.Context) error {
				gates.Add(1)
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 15*time.Second {
					t.Error("aggregate deadline absent")
				}
				return nil
			}
			got, err := g.QualifyMetadataPair(t.Context(), 20, "owner/repo", [2]int64{7, 8})
			want := "inconclusive"
			if mode == "matched" {
				want = "matched"
			}
			if err != nil || got.State != want || got.Requests != 5 || calls.Load() != 5 || gates.Load() != 5 || got.Bucket != g.quota.bucket || g.httpClient != original {
				t.Fatalf("state=%s calls=%d gates=%d result=%+v err=%v", want, calls.Load(), gates.Load(), got, err)
			}
			encoded, _ := json.Marshal(got)
			for _, secret := range []string{"qualification-secret", "owner/repo", "Metadata proof", candidateHead} {
				if strings.Contains(string(encoded), secret) {
					t.Fatal("diagnostic leaked provider data")
				}
			}
		})
	}
}

func TestMetadataQualificationFailureStopsWithoutFallback(t *testing.T) {
	for _, mode := range []string{"first_429", "pair_429", "after_429", "pair_partial", "rest_wrong_repo", "pair_malformed", "gate", "cancelled", "deadline", "parent_timeout"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c := calls.Add(1)
				if mode == "parent_timeout" {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				if (mode == "first_429" && c == 1) || (mode == "pair_429" && c == 3) || (mode == "after_429" && c == 4) {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(429)
					return
				}
				if r.URL.Path == "/graphql" {
					f := metadataPairFixture()
					if mode == "pair_partial" {
						f["data"].(map[string]any)["repository"].(map[string]any)["p1"] = nil
					}
					if mode == "pair_malformed" {
						w.Write([]byte("{}"))
						return
					}
					json.NewEncoder(w).Encode(f)
					return
				}
				n, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/pulls/"), 10, 64)
				p := qualificationMetadataREST(n)
				if mode == "rest_wrong_repo" {
					p["base"].(map[string]any)["repo"].(map[string]any)["id"] = 21
				}
				json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			defer close(release)
			g := testGitHub(server, "synthetic")
			g.quota = NewGitHubQuota()
			ctx := t.Context()
			switch mode {
			case "gate":
				g.readGate = func(context.Context) error { return errors.New("admission denied") }
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "parent_timeout":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			got, err := g.QualifyMetadataPair(ctx, 20, "owner/repo", [2]int64{7, 8})
			want := int64(1)
			switch mode {
			case "pair_429", "pair_partial", "pair_malformed":
				want = 3
			case "after_429":
				want = 4
			case "gate", "cancelled", "deadline":
				want = 0
			}
			if err == nil || got.State != "refused" || got.Requests != want || calls.Load() != want {
				t.Fatalf("result=%+v calls=%d want=%d err=%v", got, calls.Load(), want, err)
			}
			if strings.HasSuffix(mode, "429") {
				var pe *Error
				if !errors.As(err, &pe) || pe.Status != 429 {
					t.Fatalf("lost provider cooldown error: %v", err)
				}
			}
		})
	}
}

func TestMetadataQualificationTransportCapsConcurrentCallsAndDestinations(t *testing.T) {
	var calls atomic.Int64
	transport := &metadataQualificationTransport{host: "api.github.com", scheme: "https", pullPaths: [2]string{"/repos/owner/repo/pulls/7", "/repos/owner/repo/pulls/8"}, base: qualificationTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	for _, address := range []string{"https://other.example/graphql", "http://api.github.com/graphql", "https://user:password@api.github.com/graphql", "https://api.github.com/graphql?extra=true", "https://api.github.com/graphql#fragment", "https://api.github.com/repos/owner/repo/pulls/9"} {
		r, _ := http.NewRequest("POST", address, nil)
		if _, err := transport.RoundTrip(r); err == nil {
			t.Fatal("unexpected destination accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("refused destination reached base")
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequest("POST", "https://api.github.com/graphql", nil)
			response, _ := transport.RoundTrip(r)
			if response != nil {
				response.Body.Close()
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 5 || transport.calls.Load() != 5 {
		t.Fatal("request budget exceeded", calls.Load())
	}
}

func TestMetadataQualificationRejectsInvalidSelectorsBeforeTransport(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	g := testGitHub(server, "synthetic")
	for _, numbers := range [][2]int64{{7, 7}, {0, 8}, {7, -1}, {7, 2147483648}} {
		got, err := g.QualifyMetadataPair(t.Context(), 20, "owner/repo", numbers)
		if err == nil || got.State != "refused" || got.Requests != 0 {
			t.Fatal(got, err)
		}
	}
	for _, repository := range []string{"", "owner/repo/escape", "owner/repo?escape"} {
		if _, err := g.QualifyMetadataPair(t.Context(), 20, repository, [2]int64{7, 8}); err == nil {
			t.Fatal("invalid repository accepted")
		}
	}
	if _, err := g.QualifyMetadataPair(nil, 20, "owner/repo", [2]int64{7, 8}); err == nil {
		t.Fatal("nil context accepted")
	}
	var missing *GitHub
	if _, err := missing.QualifyMetadataPair(t.Context(), 20, "owner/repo", [2]int64{7, 8}); err == nil {
		t.Fatal("nil provider accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selector used transport")
	}
}

func TestMetadataQualificationComparisonRetainsOptionalPresence(t *testing.T) {
	var p Pull
	zero := int64(0)
	withZero := p
	withZero.Additions = &zero
	if reflect.DeepEqual(p, withZero) {
		t.Fatal("optional absence cannot equal known zero")
	}
}

func TestMetadataQualificationHonorsSharedGraphQLFloor(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
		_ = json.NewEncoder(w).Encode(qualificationMetadataREST(n))
	}))
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
	got, err := g.QualifyMetadataPair(t.Context(), 20, "owner/repo", [2]int64{7, 8})
	if err == nil || got.State != "refused" || got.Requests != 2 || calls.Load() != 2 {
		t.Fatalf("qualification bypassed shared quota floor: %+v err=%v calls=%d", got, err, calls.Load())
	}
}
