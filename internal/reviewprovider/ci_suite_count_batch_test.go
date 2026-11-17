package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func suiteBatchNode(head string, total int) map[string]any {
	nodes := []any{}
	if total > 0 {
		nodes = append(nodes, map[string]any{"databaseId": int64(119178679699), "commit": map[string]any{"oid": head}, "app": map[string]any{"databaseId": int64(347564)}})
	}
	return map[string]any{"__typename": "Commit", "oid": head, "checkSuites": map[string]any{"totalCount": total, "pageInfo": map[string]any{"hasNextPage": total > 1}, "nodes": nodes}}
}
func suiteBatchEnvelope(a, b any) map[string]any {
	return map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "owner/repo", "c0": a, "c1": b}}}
}
func TestSuiteCountBatchSamePhaseAndMovement(t *testing.T) {
	calls := 0
	total := 5
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/graphql" {
			var body struct {
				Query     string
				Variables map[string]string
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Query != ciSuiteCountBatchQuery || body.Variables["h0"] != ciHead || body.Variables["h1"] != ciHead || r.Method != "POST" {
				t.Error("wrong request")
			}
			json.NewEncoder(w).Encode(suiteBatchEnvelope(suiteBatchNode(ciHead, total), suiteBatchNode(ciHead, total)))
			return
		}
		json.NewEncoder(w).Encode(ciSuiteCountFixture(total))
	}))
	defer server.Close()
	g := testGitHub(server, "")
	for i := 0; i < 2; i++ {
		n, e := g.ciSuiteCount(t.Context(), ciSuitesPath, ciHead, &actionsBudget{})
		if n != 5 || e != nil {
			t.Fatal(n, e)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	calls = 0
	budgets := [2]*actionsBudget{{}, {}}
	before := g.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, budgets)
	if calls != 1 || before[0].Err != nil || before[1].Err != nil || before[0].Count != 5 {
		t.Fatal(calls, before)
	}
	total = 6
	after := g.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, budgets)
	if calls != 2 || after[0].Err != nil || after[0].Count == before[0].Count || after[1].Count == before[1].Count {
		t.Fatal(after)
	}
	// This seam deliberately returns changed counts; the existing caller must
	// reject/recollect on mismatch, never reuse the before witness as after.
	for _, b := range budgets {
		if b.pages != 2 || b.items != 2 || b.bytes == 0 {
			t.Fatal(b)
		}
	}
}
func TestSuiteCountBatchRefusals(t *testing.T) {
	for _, mode := range []string{"null", "missing", "negative", "boundary", "empty", "head", "type", "sample_head", "sample_id", "app", "fraction", "overflow", "page", "missing_page", "errors", "duplicate", "foreign", "bytes", "items", "valid_zero"} {
		t.Run(mode, func(t *testing.T) {
			a := suiteBatchNode(ciHead, 5)
			s := a["checkSuites"].(map[string]any)
			sample := s["nodes"].([]any)[0].(map[string]any)
			var left any = a
			env := suiteBatchEnvelope(a, suiteBatchNode(ciHead, 5))
			budgets := [2]*actionsBudget{{}, {}}
			switch mode {
			case "null":
				left = nil
			case "missing":
				delete(a, "oid")
			case "negative":
				s["totalCount"] = -1
			case "boundary":
				s["totalCount"] = 1000
			case "empty":
				s["nodes"] = []any{}
			case "head":
				a["oid"] = strings.Repeat("a", 40)
			case "type":
				a["__typename"] = "Tree"
			case "sample_head":
				sample["commit"] = map[string]any{"oid": strings.Repeat("a", 40)}
			case "sample_id":
				sample["databaseId"] = 0
			case "app":
				sample["app"] = nil
			case "fraction":
				sample["databaseId"] = 1.5
			case "overflow":
				sample["databaseId"] = json.Number("9223372036854775808")
			case "page":
				s["pageInfo"] = map[string]any{"hasNextPage": false}
			case "missing_page":
				delete(s, "pageInfo")
			case "errors":
				env["errors"] = []any{map[string]any{"message": "private error must not escape"}}
			case "foreign":
				env["data"].(map[string]any)["repository"].(map[string]any)["nameWithOwner"] = "other/repo"
			case "bytes":
				budgets[0].bytes = githubMaxAggregate
			case "items":
				budgets[0].items = githubMaxItems
			case "valid_zero":
				left = suiteBatchNode(ciHead, 0)
			}
			env["data"].(map[string]any)["repository"].(map[string]any)["c0"] = left
			raw, _ := json.Marshal(env)
			if mode == "duplicate" {
				raw = []byte(strings.Replace(string(raw), `"totalCount":5`, `"totalCount":5,"totalCount":5`, 1))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(raw) }))
			defer server.Close()
			out := testGitHub(server, "").ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, budgets)
			if mode == "valid_zero" {
				if out[0].Err != nil || out[0].Count != 0 {
					t.Fatal(out)
				}
				return
			}
			if out[0].Err == nil || out[0].Count != 0 {
				t.Fatal(out)
			}
			global := mode == "errors" || mode == "duplicate" || mode == "foreign"
			if (out[1].Err != nil) != global {
				t.Fatal("member isolation", out)
			}
		})
	}
}
func TestSuiteCountBatchBudgetsAndCancellation(t *testing.T) {
	calls := 0
	a := suiteBatchNode(ciHead, 5)
	a["unknown"] = strings.Repeat("x", 10000)
	b := suiteBatchNode(ciHead, 5)
	raw, _ := json.Marshal(suiteBatchEnvelope(a, b))
	ar, _ := json.Marshal(a)
	br, _ := json.Marshal(b)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write(raw) }))
	defer server.Close()
	g := testGitHub(server, "")
	budgets := [2]*actionsBudget{{}, {bytes: githubMaxAggregate - 1000}}
	out := g.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, budgets)
	if out[0].Err != nil || out[1].Err != nil {
		t.Fatal(out)
	}
	if budgets[0].bytes != len(raw)-len(br) || budgets[1].bytes != githubMaxAggregate-1000+len(raw)-len(ar) {
		t.Fatal(budgets)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out = g.ciSuiteCountsBatch(ctx, "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
	if out[0].Err == nil || calls != 1 {
		t.Fatal(out, calls)
	}
	out = g.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{pages: githubMaxPages}, {pages: githubMaxPages}})
	if out[0].Err == nil || calls != 1 {
		t.Fatal(out, calls)
	}
}

func TestSuiteCountBatchGraphQLPrimaryLimit(t *testing.T) {
	for _, mode := range []string{"proven", "success_last_point", "duplicate_resource", "duplicate_remaining", "duplicate_reset", "bad_reset", "core", "nonzero", "comma", "secondary", "duplicate_retry", "long_reset"} {
		t.Run(mode, func(t *testing.T) {
			reset := time.Now().Add(time.Hour).Truncate(time.Second)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h := w.Header()
				h.Set("X-RateLimit-Resource", "graphql")
				h.Set("X-RateLimit-Remaining", "0")
				h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				switch mode {
				case "duplicate_resource":
					h.Add("X-RateLimit-Resource", "graphql")
				case "duplicate_remaining":
					h.Add("X-RateLimit-Remaining", "0")
				case "duplicate_reset":
					h.Add("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				case "bad_reset":
					h.Set("X-RateLimit-Reset", "nonsense")
				case "core":
					h.Set("X-RateLimit-Resource", "core")
				case "nonzero":
					h.Set("X-RateLimit-Remaining", "1")
				case "comma":
					h.Set("X-RateLimit-Remaining", "0, 1")
				case "secondary":
					h.Del("X-RateLimit-Reset")
					h.Set("Retry-After", "60")
				case "duplicate_retry":
					h.Add("Retry-After", "1")
					h.Add("Retry-After", "2")
				case "long_reset":
					h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Add(48*time.Hour).Unix(), 10))
				}
				env := suiteBatchEnvelope(suiteBatchNode(ciHead, 5), suiteBatchNode(ciHead, 5))
				if mode != "success_last_point" {
					env["errors"] = []any{map[string]any{"message": "sensitive arbitrary text"}}
				}
				json.NewEncoder(w).Encode(env)
			}))
			defer server.Close()
			out := testGitHub(server, "").ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
			for _, o := range out {
				if mode == "success_last_point" {
					if o.Err != nil || o.Count != 5 {
						t.Fatal(o)
					}
					continue
				}
				var e *Error
				if !errors.As(o.Err, &e) || e.Status != 200 || strings.Contains(e.Message, "sensitive") {
					t.Fatal(o)
				}
				if mode == "proven" {
					if e.Code != "provider_rate_limited" || e.RetryTiming.State != "proven" || !e.RetryTiming.NotBefore.Equal(reset.Add(5*time.Second)) {
						t.Fatal(e)
					}
				} else if e.Code != "provider_invalid_response" {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestSuiteCountBatchExhaustedMemberUsesSingleREST(t *testing.T) {
	for _, bad := range []int{0, 1} {
		t.Run(strconv.Itoa(bad), func(t *testing.T) {
			calls := 0
			head2 := strings.Repeat("a", 40)
			heads := [2]string{ciHead, head2}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/repos/owner/repo/commits/"+heads[1-bad]+"/check-suites" {
					t.Error("unexpected request", r.Method, r.URL.Path)
				}
				fixture := ciSuiteCountFixture(5)
				fixture["check_suites"].([]map[string]any)[0]["head_sha"] = heads[1-bad]
				json.NewEncoder(w).Encode(fixture)
			}))
			defer server.Close()
			budgets := [2]*actionsBudget{{}, {}}
			budgets[bad].pages = githubMaxPages
			out := testGitHub(server, "").ciSuiteCountsBatch(t.Context(), "owner/repo", heads, budgets)
			if calls != 1 || out[bad].Err == nil || out[1-bad].Err != nil || out[1-bad].Count != 5 || budgets[bad].bytes != 0 || budgets[1-bad].pages != 1 {
				t.Fatal(calls, out, budgets)
			}
		})
	}
}
func TestSuiteCountBatchHTTPFailures(t *testing.T) {
	for _, status := range []int{403, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				w.Write([]byte("private provider body"))
			}))
			defer server.Close()
			out := testGitHub(server, "").ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
			for _, o := range out {
				var e *Error
				if !errors.As(o.Err, &e) || e.Status != status || e.RetryAfterSeconds != 60 || e.RetryTiming.State != "proven" {
					t.Fatal(o)
				}
				want := "provider_unavailable"
				if status == 403 || status == 429 {
					want = "provider_rate_limited"
				}
				if e.Code != want || strings.Contains(e.Message, "private") {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestSuiteCountBatchMidResponseCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":`))
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	out := testGitHub(server, "").ciSuiteCountsBatch(ctx, "owner/repo", [2]string{ciHead, ciHead}, [2]*actionsBudget{{}, {}})
	for _, o := range out {
		if o.Err == nil || o.Count != 0 {
			t.Fatal(o)
		}
	}
}
