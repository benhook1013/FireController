package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type ciPairFixture struct {
	mu                       sync.Mutex
	calls                    int
	counts, checks, statuses [2]int
	heads                    [2]string
	mode                     string
	cancel                   context.CancelFunc
}

func newCIPairFixture(t *testing.T, mode string) (*GitHub, *ciPairFixture) {
	t.Helper()
	f := &ciPairFixture{heads: [2]string{ciHead, strings.Repeat("a", 40)}, mode: mode}
	var server *httptest.Server
	count := func(i int) int {
		f.counts[i]++
		if i == 0 {
			switch mode {
			case "move", "byte_retry":
				if f.counts[i] >= 2 {
					return 2
				}
			case "persistent_move":
				if f.counts[i]%2 == 0 {
					return 2
				}
			case "boundary":
				return 1000
			case "distinct":
				return 0
			}
		}
		return 1
	}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.URL.Path == "/graphql" {
			var req struct{ Variables map[string]string }
			json.NewDecoder(r.Body).Decode(&req)
			if req.Variables["h0"] != f.heads[0] || req.Variables["h1"] != f.heads[1] {
				t.Error("selectors")
			}
			a, b := count(0), count(1)
			if mode == "cancel_final" && f.counts[0] == 2 {
				f.cancel()
			}
			env := suiteBatchEnvelope(suiteBatchNode(f.heads[0], a), suiteBatchNode(f.heads[1], b))
			if mode == "alias_error" {
				env["errors"] = []any{map[string]any{"path": []any{"repository", "c0"}, "message": "private failure"}}
			}
			json.NewEncoder(w).Encode(env)
			return
		}
		i := -1
		for n, h := range f.heads {
			if strings.Contains(r.URL.Path, h) {
				i = n
			}
		}
		if i < 0 {
			t.Error("wrong path", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-suites"):
			n := count(i)
			fixture := ciSuiteCountFixture(n)
			if n > 0 {
				fixture["check_suites"].([]map[string]any)[0]["head_sha"] = f.heads[i]
			}
			json.NewEncoder(w).Encode(fixture)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			f.checks[i]++
			if r.URL.Query().Get("filter") != "all" {
				t.Error("missing all")
			}
			if (mode == "body503" || mode == "retry503") && i == 0 {
				if mode == "retry503" {
					w.Header().Set("Retry-After", "0")
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(ciPairRetryFloor.Unix(), 10))
				}
				w.WriteHeader(503)
				return
			}
			total := 1
			start, end := 0, 1
			if mode == "pagination" {
				total = 101
				end = 100
				if r.URL.Query().Get("page") == "2" {
					start, end = 100, 101
				} else {
					w.Header().Set("Link", fmt.Sprintf("<%s%s?page=2&per_page=100&filter=all>; rel=next", server.URL, r.URL.Path))
				}
			}
			rows := []map[string]any{}
			for n := start; n < end; n++ {
				c := ciCheckFixture(133844910684 + int64(n))
				c["head_sha"] = f.heads[i]
				if mode == "byte_retry" && i == 0 {
					c["padding"] = strings.Repeat("x", 5<<20)
				}
				rows = append(rows, c)
			}
			if mode == "malformed" && i == 0 {
				rows[0]["app"] = nil
			}
			json.NewEncoder(w).Encode(ciEnvelope(rows, total))
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			f.statuses[i]++
			a, b := ciStatusFixture(119178679699), ciStatusFixture(119178679698)
			b["state"] = "failure"
			if mode == "byte_retry" && i == 0 {
				a["padding"] = strings.Repeat("y", 4<<20)
			}
			json.NewEncoder(w).Encode([]any{a, b})
		default:
			t.Error("wrong endpoint")
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return testGitHub(server, ""), f
}
func TestCIResultsPairEquivalence(t *testing.T) {
	for _, mode := range []string{"stable", "pagination", "move", "persistent_move", "boundary", "distinct", "body503", "retry503", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			g, old := newCIPairFixture(t, mode)
			var want [2]ciResultsPairResult
			for i, h := range old.heads {
				want[i].Inventory, want[i].Err = g.CIResults(t.Context(), "owner/repo", h)
				want[i].Inventory.ObservedAt = time.Time{}
			}
			g, f := newCIPairFixture(t, mode)
			got := g.ciResultsPair(t.Context(), "owner/repo", f.heads)
			for i := range got {
				if got[i].Err == nil && got[i].Inventory.ObservedAt.IsZero() {
					t.Fatal("missing time")
				}
				got[i].Inventory.ObservedAt = time.Time{}
				if !reflect.DeepEqual(got[i].Inventory, want[i].Inventory) || !reflect.DeepEqual(ciPairErrorFacts(t, got[i].Err), ciPairErrorFacts(t, want[i].Err)) {
					t.Fatalf("member%d got%+v want%+v", i, got[i], want[i])
				}
			}
			if mode == "retry503" {
				var e *Error
				if !errors.As(got[0].Err, &e) || e.Code != "provider_unavailable" || e.Status != 503 || !e.retryDirected || e.RetryTiming.State != "proven" || !e.RetryTiming.NotBefore.Equal(ciPairRetryFloor) {
					t.Fatal("retry contract", got[0].Err)
				}
			}
			if mode == "stable" {
				if old.calls != 8 || f.calls != 6 {
					t.Fatal(old.calls, f.calls)
				}
			}
			if mode == "pagination" {
				if old.calls != 10 || f.calls != 8 {
					t.Fatal(old.calls, f.calls)
				}
			}
			if mode == "move" || mode == "persistent_move" {
				if f.checks != [2]int{2, 1} || f.statuses != [2]int{2, 1} || f.counts != [2]int{4, 2} {
					t.Fatal(f)
				}
			}
		})
	}
}
func TestCIResultsPairCancellationDiscardsProvisionalInventories(t *testing.T) {
	g, f := newCIPairFixture(t, "cancel_final")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.cancel = cancel
	got := g.ciResultsPair(ctx, "owner/repo", f.heads)
	for _, o := range got {
		if o.Err == nil || !reflect.DeepEqual(o.Inventory, CIResultInventory{}) {
			t.Fatal(o)
		}
	}
	if f.checks != [2]int{1, 1} || f.statuses != [2]int{1, 1} || f.calls != 6 {
		t.Fatal(f)
	}
}

func TestCIResultsPairRetryKeepsByteBudget(t *testing.T) {
	g, f := newCIPairFixture(t, "byte_retry")
	out := g.ciResultsPair(t.Context(), "owner/repo", f.heads)
	if out[0].Err == nil || !reflect.DeepEqual(out[0].Inventory, CIResultInventory{}) || out[1].Err != nil {
		t.Fatal(out)
	}
	if f.checks != [2]int{2, 1} || f.statuses != [2]int{2, 1} {
		t.Fatal(f)
	}
}

func TestCIResultsPairAliasErrorConservativelyRefusesBoth(t *testing.T) {
	g, f := newCIPairFixture(t, "alias_error")
	out := g.ciResultsPair(t.Context(), "owner/repo", f.heads)
	for _, o := range out {
		if o.Err == nil || !reflect.DeepEqual(o.Inventory, CIResultInventory{}) {
			t.Fatal(o)
		}
	}
	if f.calls != 1 || f.checks != [2]int{} || f.statuses != [2]int{} {
		t.Fatal(f)
	}
	// Independent REST is deliberately more available for this GraphQL-only
	// failure. The batching seam makes no identical failure-surface claim.
	for _, h := range f.heads {
		if _, err := g.CIResults(t.Context(), "owner/repo", h); err != nil {
			t.Fatal(err)
		}
	}
}

var ciPairRetryFloor = time.Now().UTC().Truncate(time.Hour).Add(2 * time.Hour)

func ciPairErrorFacts(t *testing.T, err error) any {
	t.Helper()
	if err == nil {
		return nil
	}
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("unexpected error type %T", err)
	}
	facts := *failure
	// Observation clock is not error identity. The provider floor, when supplied
	// by fixed reset headers, must remain identical, as must all other fields.
	facts.RetryTiming.ObservedAt = time.Time{}
	return facts
}
