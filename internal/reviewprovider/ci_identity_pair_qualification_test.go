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

func ciIdentityFixture() map[string]any {
	f := metadataPairFixture()
	repo := f["data"].(map[string]any)["repository"].(map[string]any)
	for _, k := range []string{"p0", "p1"} {
		repo[k].(map[string]any)["potentialMergeCommit"] = map[string]any{"oid": candidateMerge}
	}
	p1 := repo["p1"].(map[string]any)
	p1["headRefOid"] = strings.Repeat("e", 40)
	p1["potentialMergeCommit"] = map[string]any{"oid": strings.Repeat("f", 40)}
	return f
}
func TestCIIdentityQualification(t *testing.T) {
	modes := []string{"matched", "large_repo", "before_null", "before_unknown", "before_conflict", "before_closed", "before_merged", "pair_null", "pair_unknown", "pair_conflict", "pair_closed", "pair_merged", "pair_moved", "pair_swapped", "after_moved", "after_closed", "omitted", "invalid_sha", "wrong_repo", "partial", "malformed", "limit", "gate", "cancel"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			var calls, gates atomic.Int64
			id := int64(20)
			if mode == "large_repo" {
				id = 5000000000
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c := calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer synthetic-secret" {
					t.Error("credential lost")
				}
				if r.URL.Path == "/graphql" {
					if mode == "limit" {
						w.Header().Set("Retry-After", "60")
						w.WriteHeader(429)
						return
					}
					if mode == "malformed" {
						w.Write([]byte("{}"))
						return
					}
					f := ciIdentityFixture()
					repo := f["data"].(map[string]any)["repository"].(map[string]any)
					repo["fullDatabaseId"] = strconv.FormatInt(id, 10)
					for _, k := range []string{"p0", "p1"} {
						p := repo[k].(map[string]any)
						p["baseRepository"].(map[string]any)["fullDatabaseId"] = strconv.FormatInt(id, 10)
						p["headRepository"].(map[string]any)["fullDatabaseId"] = strconv.FormatInt(id, 10)
					}
					p := repo["p0"].(map[string]any)
					switch mode {
					case "pair_null":
						p["potentialMergeCommit"] = nil
					case "pair_unknown":
						p["mergeable"] = "UNKNOWN"
					case "pair_conflict":
						p["mergeable"] = "CONFLICTING"
					case "pair_closed":
						p["state"] = "CLOSED"
					case "pair_merged":
						p["state"] = "MERGED"
						p["merged"] = true
						p["mergedAt"] = "2026-11-01T00:00:00Z"
						p["potentialMergeCommit"] = nil
					case "pair_swapped":
						p1 := repo["p1"].(map[string]any)
						p["potentialMergeCommit"], p1["potentialMergeCommit"] = p1["potentialMergeCommit"], p["potentialMergeCommit"]
					case "pair_moved":
						p["potentialMergeCommit"] = map[string]any{"oid": strings.Repeat("d", 40)}
					case "omitted":
						delete(p, "potentialMergeCommit")
					case "invalid_sha":
						p["potentialMergeCommit"] = map[string]any{"oid": strings.Repeat("C", 40)}
					case "wrong_repo":
						repo["fullDatabaseId"] = "21"
					case "partial":
						repo["p1"] = nil
					}
					_ = json.NewEncoder(w).Encode(f)
					return
				}
				n, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
				p := qualificationMetadataREST(n)
				p["merge_commit_sha"] = candidateMerge
				if n == 8 {
					p["head"].(map[string]any)["sha"] = strings.Repeat("e", 40)
					p["merge_commit_sha"] = strings.Repeat("f", 40)
				}
				p["base"].(map[string]any)["repo"].(map[string]any)["id"] = id
				p["head"].(map[string]any)["repo"].(map[string]any)["id"] = id
				if c <= 2 {
					switch mode {
					case "before_null":
						p["merge_commit_sha"] = nil
					case "before_unknown":
						p["mergeable"] = nil
					case "before_conflict":
						p["mergeable"] = false
					case "before_closed":
						p["state"] = "closed"
					case "before_merged":
						p["state"] = "closed"
						p["merged"] = true
						p["merged_at"] = "2026-11-01T00:00:00Z"
					}
				} else {
					if mode == "after_moved" {
						p["merge_commit_sha"] = strings.Repeat("d", 40)
					}
					if mode == "after_closed" {
						p["state"] = "closed"
					}
				}
				_ = json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			g := testGitHub(server, "synthetic-secret")
			g.quota = NewGitHubQuota()
			g.readGate = func(ctx context.Context) error {
				gates.Add(1)
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 15*time.Second {
					t.Error("missing aggregate deadline")
				}
				if mode == "gate" {
					return errors.New("fixture gate")
				}
				return nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			result, err := g.QualifyCIIdentityPair(ctx, id, "owner/repo", [2]int64{7, 8})
			want, count, wantErr := "inconclusive", int64(5), false
			switch mode {
			case "matched", "large_repo":
				want = "matched"
			case "before_null", "before_unknown", "before_conflict", "before_closed", "before_merged":
				count = 2
			case "omitted", "invalid_sha", "wrong_repo", "partial", "malformed", "limit":
				want = "refused"
				count = 3
				wantErr = true
			case "gate", "cancel":
				want = "refused"
				count = 0
				wantErr = true
			}
			if result.State != want || result.Requests != count || calls.Load() != count || (err != nil) != wantErr {
				t.Fatalf("%+v err=%v calls=%d want=%s/%d", result, err, calls.Load(), want, count)
			}
			if mode == "limit" {
				var e *Error
				if !errors.As(err, &e) || e.Status != 429 {
					t.Fatal("lost cooldown", err)
				}
			}
			if mode == "matched" && gates.Load() != 5 {
				t.Fatal("lost admission", gates.Load())
			}
			b, _ := json.Marshal(result)
			for _, secret := range []string{"synthetic-secret", "owner/repo", candidateMerge} {
				if strings.Contains(string(b), secret) {
					t.Fatal("diagnostic leak")
				}
			}
		})
	}
}
func TestCIIdentityQualificationSelectors(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	g := testGitHub(server, "synthetic")
	for _, numbers := range [][2]int64{{7, 7}, {0, 8}, {-1, 8}, {7, 2147483648}} {
		r, e := g.QualifyCIIdentityPair(t.Context(), 20, "owner/repo", numbers)
		if e == nil || r.State != "refused" || r.Requests != 0 {
			t.Fatal(r, e)
		}
	}
	if calls.Load() != 0 {
		t.Fatal(calls.Load())
	}
}

func TestCIIdentityQualificationQuotaAndDeadline(t *testing.T) {
	for _, mode := range []string{"quota", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "deadline" {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				n, _ := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
				p := qualificationMetadataREST(n)
				p["merge_commit_sha"] = candidateMerge
				_ = json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			defer close(release)
			g := testGitHub(server, "synthetic")
			g.quota = NewGitHubQuota()
			ctx := t.Context()
			if mode == "quota" {
				h := http.Header{}
				h.Set("X-RateLimit-Resource", "graphql")
				h.Set("X-RateLimit-Remaining", "0")
				h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
				failure := ciSuiteBatchPrimaryLimit(h)
				if failure == nil {
					t.Fatal("fixture")
				}
				g.quota.observeSuiteGraphQLPrimary(ctx, http.StatusOK, failure.RetryTiming)
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			r, e := g.QualifyCIIdentityPair(ctx, 20, "owner/repo", [2]int64{7, 8})
			want := int64(2)
			if mode == "deadline" {
				want = 1
			}
			if e == nil || r.State != "refused" || r.Requests != want || calls.Load() != want {
				t.Fatalf("%+v err=%v calls=%d", r, e, calls.Load())
			}
		})
	}
}
