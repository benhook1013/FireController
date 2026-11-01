package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const ciHead = "1234567890abcdef1234567890abcdef12345678"
const ciChecksPath = "/repos/owner/repo/commits/" + ciHead + "/check-runs"
const ciStatusesPath = "/repos/owner/repo/commits/" + ciHead + "/statuses"
const ciSuitesPath = "/repos/owner/repo/commits/" + ciHead + "/check-suites"

func ciSuiteCountFixture(total int) map[string]any {
	rows := []map[string]any{}
	if total > 0 {
		rows = append(rows, map[string]any{"id": 20, "head_sha": ciHead, "app": map[string]any{"id": 10}})
	}
	return map[string]any{"total_count": total, "check_suites": rows}
}

func TestCIResultsSuiteTruncationBoundaryBeforeChecks(t *testing.T) {
	for _, total := range []int{1000, 1001} {
		for _, reportedChecks := range []int{0, 1000} {
			t.Run(fmt.Sprintf("suites%d/checks%d", total, reportedChecks), func(t *testing.T) {
				var origin string
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path == ciSuitesPath {
						_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(total))
						return
					}
					if r.URL.Path == ciChecksPath {
						// This internally consistent truncated result must never be read.
						page, _ := strconv.Atoi(r.URL.Query().Get("page"))
						rows := []map[string]any{}
						if reportedChecks > 0 {
							for i := 0; i < 100; i++ {
								rows = append(rows, ciCheckFixture(int64((page-1)*100+i+1)))
							}
							if page < 10 {
								w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100&filter=all>; rel=next", origin, ciChecksPath, page+1))
							}
						}
						_ = json.NewEncoder(w).Encode(ciEnvelope(rows, reportedChecks))
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				}))
				defer server.Close()
				origin = server.URL
				got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
				assertNoCIInventory(t, got, err)
				if calls.Load() != 1 {
					t.Fatal("read potentially truncated inventory", calls.Load())
				}
			})
		}
	}
}

func TestCIResultsSuiteCountsBelowBoundaryAndExactSelectors(t *testing.T) {
	for _, total := range []int{0, 999} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			var origin string
			var paths []string
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.RequestURI())
				mu.Unlock()
				if r.Method != http.MethodGet {
					t.Error("suite count changed method")
				}
				switch r.URL.Path {
				case ciSuitesPath:
					if r.URL.RawQuery != "page=1&per_page=1" {
						t.Error("count selector changed", r.URL.String())
					}
					if total > 1 {
						w.Header().Set("Link", "<"+origin+ciSuitesPath+"?page=2&per_page=1>; rel=next, <"+origin+ciSuitesPath+"?page=999&per_page=1>; rel=last")
					}
					_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(total))
				case ciChecksPath:
					rows := []map[string]any{}
					if total > 0 {
						rows = append(rows, ciCheckFixture(1))
					}
					_ = json.NewEncoder(w).Encode(ciEnvelope(rows, len(rows)))
				case ciStatusesPath:
					_, _ = w.Write([]byte(`[]`))
				default:
					t.Error("enumerated suites or unrelated resource", r.URL.String())
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
			if err != nil || got.Checks == nil || got.Statuses == nil || total == 0 && len(got.Checks) != 0 || total == 999 && len(got.Checks) != 1 {
				t.Fatal("safe count boundary rejected", got, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(paths) != 4 || !strings.HasPrefix(paths[0], ciSuitesPath+"?") || !strings.HasPrefix(paths[1], ciChecksPath+"?") || !strings.HasPrefix(paths[2], ciStatusesPath+"?") || !strings.HasPrefix(paths[3], ciSuitesPath+"?") {
				t.Fatal("count observations did not bracket collection", paths)
			}
		})
	}
}

func TestCIResultsMalformedSuiteCountFailsBeforeChecksOrDiscardsCompletedReads(t *testing.T) {
	valid := ciJSON(ciSuiteCountFixture(1))
	cases := map[string]string{
		"missing_total":   `{"check_suites":[]}`,
		"null_total":      `{"total_count":null,"check_suites":[]}`,
		"negative_total":  `{"total_count":-1,"check_suites":[]}`,
		"oversize_total":  `{"total_count":10001,"check_suites":[]}`,
		"fraction_total":  `{"total_count":1.0,"check_suites":[]}`,
		"string_total":    `{"total_count":"1","check_suites":[]}`,
		"duplicate_total": `{"total_count":0,"total_count":0,"check_suites":[]}`,
		"missing_suites":  `{"total_count":0}`,
		"null_suites":     `{"total_count":0,"check_suites":null}`,
		"positive_empty":  `{"total_count":1,"check_suites":[]}`,
		"zero_with_row":   strings.Replace(valid, `"total_count":1`, `"total_count":0`, 1),
		"two_rows":        ciJSON(map[string]any{"total_count": 2, "check_suites": []any{ciSuiteCountFixture(1)["check_suites"].([]map[string]any)[0], ciSuiteCountFixture(1)["check_suites"].([]map[string]any)[0]}}),
		"null_row":        `{"total_count":1,"check_suites":[null]}`,
		"wrong_head":      strings.Replace(valid, ciHead, strings.Repeat("b", 40), 1),
		"uppercase_head":  strings.Replace(valid, ciHead, strings.ToUpper(ciHead), 1),
		"zero_id":         strings.Replace(valid, `"id":20`, `"id":0`, 1),
		"null_id":         strings.Replace(valid, `"id":20`, `"id":null`, 1),
		"missing_head":    strings.Replace(valid, `"head_sha":"`+ciHead+`",`, "", 1),
		"null_app":        strings.Replace(valid, `"app":{"id":10}`, `"app":null`, 1),
		"missing_app_id":  strings.Replace(valid, `"app":{"id":10}`, `"app":{}`, 1),
		"bad_app_id":      strings.Replace(valid, `"app":{"id":10}`, `"app":{"id":-1}`, 1),
		"app_id_alias":    strings.Replace(valid, `"app":{"id":10}`, `"app":{"ID":10}`, 1),
	}
	for name, body := range cases {
		for _, final := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/final=%v", name, final), func(t *testing.T) {
				var calls, suiteCalls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					switch r.URL.Path {
					case ciSuitesPath:
						n := suiteCalls.Add(1)
						if !final || n == 2 {
							_, _ = w.Write([]byte(body))
						} else {
							_, _ = w.Write([]byte(valid))
						}
					case ciChecksPath:
						_ = json.NewEncoder(w).Encode(ciEnvelope([]map[string]any{ciCheckFixture(1)}, 1))
					case ciStatusesPath:
						_, _ = w.Write([]byte(`[]`))
					default:
						t.Error("wrong selector", r.URL.String())
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
				assertNoCIInventory(t, got, err)
				want := int64(1)
				if final {
					want = 4
				}
				if calls.Load() != want {
					t.Fatal("continued past invalid count or skipped bracketing", calls.Load(), want)
				}
			})
		}
	}
}

func TestCIResultsSuiteCountDriftAndMoreDistinctResultSuitesFailClosed(t *testing.T) {
	for _, mode := range []string{"increase", "decrease", "final_boundary", "zero_count_with_check", "distinct_exceeds_count"} {
		t.Run(mode, func(t *testing.T) {
			var suiteCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case ciSuitesPath:
					n := suiteCalls.Add(1)
					total := 1
					if mode == "zero_count_with_check" {
						total = 0
					}
					if n%2 == 0 {
						switch mode {
						case "increase":
							total = 2
						case "decrease":
							total = 0
						case "final_boundary":
							total = 1000
						}
					}
					_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(total))
				case ciChecksPath:
					rows := []map[string]any{ciCheckFixture(1)}
					if mode == "distinct_exceeds_count" {
						other := ciCheckFixture(2)
						other["check_suite"] = map[string]any{"id": 21}
						rows = append(rows, other)
					}
					_ = json.NewEncoder(w).Encode(ciEnvelope(rows, len(rows)))
				case ciStatusesPath:
					_, _ = w.Write([]byte(`[]`))
				default:
					t.Error("wrong selector")
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
			assertNoCIInventory(t, got, err)
			want := int64(2)
			if mode == "increase" || mode == "decrease" {
				want = 4
				if !hasCode(err, "provider_observation_changed") {
					t.Fatal("movement lost its diagnostic", err)
				}
			}
			if suiteCalls.Load() != want {
				t.Fatal("final count not observed", suiteCalls.Load())
			}
		})
	}
}

func TestCIResultsSuiteCountNextLinkNeverChangesSelectedScope(t *testing.T) {
	for _, mode := range []string{"foreign", "head", "page", "per_page", "duplicate", "extra", "zero_total_next", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			var origin string
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				link := origin + ciSuitesPath + "?page=2&per_page=1"
				total := 2
				switch mode {
				case "foreign":
					link = "https://elsewhere.invalid" + ciSuitesPath + "?page=2&per_page=1"
				case "head":
					link = strings.Replace(link, ciHead, strings.Repeat("b", 40), 1)
				case "page":
					link = strings.Replace(link, "page=2", "page=3", 1)
				case "per_page":
					link = strings.Replace(link, "per_page=1", "per_page=100", 1)
				case "extra":
					link += "&app_id=10"
				case "zero_total_next":
					total = 0
				}
				header := "<" + link + ">; rel=next"
				if mode == "duplicate" {
					header += ", " + header
				}
				if mode == "malformed" {
					header = "not a link"
				}
				w.Header().Set("Link", header)
				_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(total))
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
			assertNoCIInventory(t, got, err)
			if calls.Load() != 1 {
				t.Fatal("untrusted count Link followed", calls.Load())
			}
		})
	}
}

func TestCIResultsFinalSuiteCountPermissionCancellationAndSharedBudgets(t *testing.T) {
	t.Run("permission", func(t *testing.T) {
		var suites atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case ciSuitesPath:
				if suites.Add(1) == 2 {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"message":"PRIVATE_DIAGNOSTIC"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
			case ciChecksPath:
				_ = json.NewEncoder(w).Encode(ciEnvelope([]map[string]any{ciCheckFixture(1)}, 1))
			case ciStatusesPath:
				_, _ = w.Write([]byte(`[]`))
			}
		}))
		defer server.Close()
		got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
	})
	t.Run("cancel_final_count", func(t *testing.T) {
		var suites, calls atomic.Int64
		entered, exited := make(chan struct{}), make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			switch r.URL.Path {
			case ciSuitesPath:
				if suites.Add(1) == 2 {
					close(entered)
					<-r.Context().Done()
					close(exited)
					return
				}
				_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
			case ciChecksPath:
				_ = json.NewEncoder(w).Encode(ciEnvelope([]map[string]any{ciCheckFixture(1)}, 1))
			case ciStatusesPath:
				_, _ = w.Write([]byte(`[]`))
			}
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type answer struct {
			result CIResultInventory
			err    error
		}
		done := make(chan answer, 1)
		go func() {
			result, err := testGitHub(server, "").CIResults(ctx, "owner/repo", ciHead)
			done <- answer{result, err}
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("final count not entered")
		}
		cancel()
		select {
		case a := <-done:
			assertNoCIInventory(t, a.result, a.err)
		case <-time.After(time.Second):
			t.Fatal("count cancellation unbounded")
		}
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Fatal("count request left open")
		}
		if calls.Load() != 4 {
			t.Fatal("count retried", calls.Load())
		}
	})
	t.Run("final_count_bytes", func(t *testing.T) {
		var calls atomic.Int64
		padding := strings.Repeat("x", 7<<20)
		checkPadding := strings.Repeat("x", 3<<20)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			switch r.URL.Path {
			case ciSuitesPath:
				body := ciSuiteCountFixture(1)
				body["ignored_metadata"] = padding
				_ = json.NewEncoder(w).Encode(body)
			case ciChecksPath:
				body := ciEnvelope([]map[string]any{ciCheckFixture(1)}, 1)
				body["ignored_metadata"] = checkPadding
				_ = json.NewEncoder(w).Encode(body)
			case ciStatusesPath:
				_, _ = w.Write([]byte(`[]`))
			}
		}))
		defer server.Close()
		got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
		if calls.Load() != 4 {
			t.Fatal("final count not charged to shared byte budget", calls.Load())
		}
	})
	t.Run("count_items_and_pages", func(t *testing.T) {
		g, calls := ciFixtureServer(t, `{"total_count":0,"check_runs":[]}`, `[]`)
		budget := &actionsBudget{items: 9999}
		if _, err := g.ciSuiteCount(t.Context(), ciSuitesPath, ciHead, budget); err != nil || budget.items != 10000 {
			t.Fatal("count item not charged", budget, err)
		}
		if _, err := g.ciSuiteCount(t.Context(), ciSuitesPath, ciHead, budget); err == nil {
			t.Fatal("count item overflow accepted")
		}
		before := calls.Load()
		if _, err := g.ciSuiteCount(t.Context(), ciSuitesPath, ciHead, &actionsBudget{pages: 100}); err == nil || calls.Load() != before {
			t.Fatal("count page overflow reached provider")
		}
	})
}

func ciCheckFixture(id int64) map[string]any {
	return map[string]any{"id": id, "name": " build [linux] ", "head_sha": ciHead, "status": "completed", "conclusion": "success", "started_at": "2026-01-01T00:00:01+00:00", "completed_at": "2026-01-01T00:00:02.123Z", "app": map[string]any{"id": 10, "slug": "not-an-identity"}, "check_suite": map[string]any{"id": 20}, "output": map[string]any{"text": "PRIVATE_OUTPUT"}}
}

func ciStatusFixture(id int64) map[string]any {
	return map[string]any{"id": id, "context": " build [linux] ", "state": "success", "created_at": "2026-01-01T00:00:01Z", "updated_at": "2026-01-01T00:00:02Z", "creator": map[string]any{"id": 10}, "target_url": "PRIVATE_TARGET"}
}

func ciEnvelope(rows []map[string]any, total int) map[string]any {
	return map[string]any{"total_count": total, "check_runs": rows}
}

func ciFixtureServer(t *testing.T, checks, statuses string) (*GitHub, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Error("inventory changed method", r.Method)
		}
		switch r.URL.Path {
		case ciSuitesPath:
			_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
		case ciChecksPath:
			_, _ = w.Write([]byte(checks))
		case ciStatusesPath:
			_, _ = w.Write([]byte(statuses))
		default:
			t.Error("unrelated inventory request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return testGitHub(server, "synthetic-ci-token"), calls
}

func ciJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func assertNoCIInventory(t *testing.T, result CIResultInventory, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(result, CIResultInventory{}) || strings.Contains(err.Error(), "PRIVATE_") || strings.Contains(err.Error(), "synthetic-ci-token") {
		t.Fatal("failed inventory returned partial facts or unsafe error", result, err)
	}
}

func TestCIResultsExactHeadAllPagesRetainHistoryAndAppIdentity(t *testing.T) {
	var origin string
	var paths []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-ci-token" {
			t.Error("method/credential changed")
		}
		q := r.URL.Query()
		if r.URL.Path == ciSuitesPath {
			if len(q) != 2 || q.Get("per_page") != "1" || q.Get("page") != "1" {
				t.Error("suite count selector changed", r.URL.String())
			}
			_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
			return
		}
		page := q.Get("page")
		if q.Get("per_page") != "100" || page != "1" && page != "2" {
			t.Error("page selection changed", r.URL.String())
		}
		switch r.URL.Path {
		case ciChecksPath:
			if len(q) != 3 || q.Get("filter") != "all" {
				t.Error("all filter lost", r.URL.String())
			}
			row := ciCheckFixture(1)
			if page == "1" {
				w.Header().Set("Link", "<"+origin+ciChecksPath+"?page=2&per_page=100&filter=all>; rel=next")
			} else {
				row = ciCheckFixture(2)
				row["app"] = map[string]any{"id": 11, "slug": "not-an-identity"}
				row["conclusion"] = "failure"
			}
			_ = json.NewEncoder(w).Encode(ciEnvelope([]map[string]any{row}, 2))
		case ciStatusesPath:
			if len(q) != 2 {
				t.Error("invented status selector", r.URL.String())
			}
			row := ciStatusFixture(4)
			if page == "1" {
				w.Header().Set("Link", "<"+origin+ciStatusesPath+"?page=2&per_page=100>; rel=next")
			} else {
				row = ciStatusFixture(3)
				row["state"] = "failure"
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{row})
		default:
			t.Error("unrelated scope", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	origin = server.URL
	before := time.Now()
	got, err := testGitHub(server, "synthetic-ci-token").CIResults(t.Context(), "owner/repo", ciHead)
	if err != nil || got.Repository != "owner/repo" || got.HeadSHA != ciHead || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now()) || got.ObservedAt.Location() != time.UTC || len(got.Checks) != 2 || len(got.Statuses) != 2 {
		t.Fatal("selected inventory incomplete", got, err)
	}
	if got.Checks[0].AppID != 10 || got.Checks[1].AppID != 11 || got.Checks[1].Conclusion != "failure" || got.Checks[0].SuiteID != 20 || got.Checks[0].Name != " build [linux] " || got.Checks[0].StartedAt.Location() != time.UTC || got.Statuses[0].ID != 4 || got.Statuses[1].ID != 3 || got.Statuses[1].State != "failure" || got.Statuses[0].Context != " build [linux] " {
		t.Fatal("app identity/context/history flattened", got)
	}
	body := ciJSON(got)
	for _, hidden := range []string{"PRIVATE_OUTPUT", "PRIVATE_TARGET", "synthetic-ci-token", "creator", "slug", "latest", "ready"} {
		if strings.Contains(body, hidden) {
			t.Fatal("DTO gained unrelated/verdict fields", hidden)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 6 || !strings.HasPrefix(paths[0], ciSuitesPath+"?") || !strings.HasPrefix(paths[5], ciSuitesPath+"?") {
		t.Fatal("unexpected request count", paths)
	}
}

func TestCIResultsProvenEmptyAndLifecycleFacts(t *testing.T) {
	g, calls := ciFixtureServer(t, `{"total_count":0,"check_runs":[]}`, `[]`)
	got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
	if err != nil || got.Checks == nil || got.Statuses == nil || len(got.Checks)+len(got.Statuses) != 0 || calls.Load() != 4 {
		t.Fatal("proven empty was not explicit", got, err)
	}
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested", "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure"} {
		t.Run(status, func(t *testing.T) {
			row := ciCheckFixture(1)
			switch status {
			case "queued", "in_progress", "waiting", "pending", "requested":
				row["status"], row["conclusion"], row["completed_at"] = status, nil, nil
				if status != "in_progress" {
					row["started_at"] = nil
				}
			default:
				row["conclusion"] = status
				if status == "skipped" || status == "cancelled" {
					row["started_at"] = nil
				}
			}
			g, _ := ciFixtureServer(t, ciJSON(ciEnvelope([]map[string]any{row}, 1)), `[]`)
			got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
			if err != nil || len(got.Checks) != 1 || got.Checks[0].Status != row["status"] {
				t.Fatal("valid lifecycle rejected", status, got, err)
			}
		})
	}
	for _, state := range []string{"error", "failure", "pending", "success"} {
		row := ciStatusFixture(1)
		row["state"] = state
		g, _ := ciFixtureServer(t, `{"total_count":0,"check_runs":[]}`, ciJSON([]map[string]any{row}))
		got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
		if err != nil || len(got.Statuses) != 1 || got.Statuses[0].State != state {
			t.Fatal("status fact rejected", got, err)
		}
	}
}

func TestCIResultsInvalidCheckFactsFailWholeInventory(t *testing.T) {
	cases := map[string]func(map[string]any){
		"wrong_head":                func(r map[string]any) { r["head_sha"] = strings.Repeat("a", 40) },
		"upper_head":                func(r map[string]any) { r["head_sha"] = strings.ToUpper(ciHead) },
		"id_zero":                   func(r map[string]any) { r["id"] = 0 },
		"id_decimal":                func(r map[string]any) { r["id"] = 1.5 },
		"id_string":                 func(r map[string]any) { r["id"] = "1" },
		"id_missing":                func(r map[string]any) { delete(r, "id") },
		"app_null":                  func(r map[string]any) { r["app"] = nil },
		"app_zero":                  func(r map[string]any) { r["app"] = map[string]any{"id": 0} },
		"app_missing_id":            func(r map[string]any) { r["app"] = map[string]any{"slug": "github-actions"} },
		"suite_negative":            func(r map[string]any) { r["check_suite"] = map[string]any{"id": -1} },
		"suite_alias":               func(r map[string]any) { delete(r, "check_suite"); r["checkSuite"] = map[string]any{"id": 20} },
		"name_empty":                func(r map[string]any) { r["name"] = " " },
		"name_control":              func(r map[string]any) { r["name"] = "build\u0085" },
		"name_long":                 func(r map[string]any) { r["name"] = strings.Repeat("x", 256) },
		"unknown_status":            func(r map[string]any) { r["status"] = "future" },
		"unknown_conclusion":        func(r map[string]any) { r["conclusion"] = "passed" },
		"completed_null_conclusion": func(r map[string]any) { r["conclusion"] = nil },
		"missing_conclusion":        func(r map[string]any) { delete(r, "conclusion") },
		"completed_no_end":          func(r map[string]any) { r["completed_at"] = nil },
		"missing_start":             func(r map[string]any) { delete(r, "started_at") },
		"invalid_start":             func(r map[string]any) { r["started_at"] = "yesterday" },
		"reversed":                  func(r map[string]any) { r["started_at"] = "2026-01-02T00:00:00Z" },
		"zero_end":                  func(r map[string]any) { r["completed_at"] = "0001-01-01T00:00:00Z" },
		"pending_end":               func(r map[string]any) { r["status"], r["conclusion"] = "queued", nil },
		"pending_conclusion":        func(r map[string]any) { r["status"], r["completed_at"] = "queued", nil },
		"running_no_start": func(r map[string]any) {
			r["status"], r["conclusion"], r["completed_at"], r["started_at"] = "in_progress", nil, nil, nil
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			row := ciCheckFixture(1)
			change(row)
			g, calls := ciFixtureServer(t, ciJSON(ciEnvelope([]map[string]any{row}, 1)), `[]`)
			got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
			assertNoCIInventory(t, got, err)
			if calls.Load() != 2 {
				t.Fatal("continued after invalid checks", calls.Load())
			}
		})
	}
}

func TestCIResultsMalformedAndStatusFactsFailWholeInventory(t *testing.T) {
	checks := ciJSON(ciEnvelope([]map[string]any{ciCheckFixture(1)}, 1))
	for name, body := range map[string]string{
		"null_list": `null`, "object_list": `{}`, "null_item": `[null]`, "duplicate_key": `[{"id":1,"id":2}]`, "unicode": `[{"id":1,"context":"\ud800"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			g, _ := ciFixtureServer(t, checks, body)
			got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
			assertNoCIInventory(t, got, err)
		})
	}
	for _, field := range []string{"id", "context", "state", "created_at", "updated_at"} {
		for _, mode := range []string{"missing", "null", "wrongtype"} {
			t.Run(field+"_"+mode, func(t *testing.T) {
				row := ciStatusFixture(1)
				if mode == "missing" {
					delete(row, field)
				} else if mode == "null" {
					row[field] = nil
				} else {
					row[field] = false
				}
				g, _ := ciFixtureServer(t, checks, ciJSON([]map[string]any{row}))
				got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
				assertNoCIInventory(t, got, err)
			})
		}
	}
	for _, change := range []func(map[string]any){
		func(r map[string]any) { r["id"] = -1 },
		func(r map[string]any) { r["context"] = "build\n" },
		func(r map[string]any) { r["context"] = strings.Repeat("x", 256) },
		func(r map[string]any) { r["state"] = "completed" },
		func(r map[string]any) { r["created_at"] = "tomorrow" },
		func(r map[string]any) { r["updated_at"] = "2025-01-01T00:00:00Z" },
	} {
		row := ciStatusFixture(1)
		change(row)
		g, _ := ciFixtureServer(t, checks, ciJSON([]map[string]any{row}))
		got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
	}
	for _, body := range []string{`null`, `{}`, `{"total_count":0}`, `{"total_count":0,"check_runs":null}`, `{"total_count":-1,"check_runs":[]}`, `{"total_count":1.0,"check_runs":[]}`, `{"total_count":0,"total_count":0,"check_runs":[]}`, `{"total_count":0,"Check_Runs":[]}`, `{"total_count":10001,"check_runs":[]}`, "{\"total_count\":0,\"check_runs\":[],\"extra\":\"\xff\"}"} {
		g, _ := ciFixtureServer(t, body, `[]`)
		got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
	}
}

func TestCIResultsPaginationDriftDuplicatesAndScopeFailClosed(t *testing.T) {
	for _, mode := range []string{"total_drift", "missing_rows", "duplicate_check", "conflicting_check", "duplicate_status", "conflicting_status", "filter_missing", "filter_latest", "filter_duplicate", "changed_head", "extra_selector", "loop", "foreign_origin", "partial_status", "null_second", "empty_more"} {
		t.Run(mode, func(t *testing.T) {
			var origin string
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == ciSuitesPath {
					_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
					return
				}
				page := r.URL.Query().Get("page")
				if r.URL.Path == ciChecksPath {
					row := ciCheckFixture(1)
					total := 2
					if page == "1" {
						link := origin + ciChecksPath + "?page=2&per_page=100&filter=all"
						switch mode {
						case "filter_missing":
							link = origin + ciChecksPath + "?page=2&per_page=100"
						case "filter_latest":
							link = origin + ciChecksPath + "?page=2&per_page=100&filter=latest"
						case "filter_duplicate":
							link += "&filter=all"
						case "changed_head":
							link = strings.ReplaceAll(link, ciHead, strings.Repeat("b", 40))
						case "extra_selector":
							link += "&app_id=10"
						case "loop":
							link = strings.ReplaceAll(link, "page=2", "page=1")
						case "foreign_origin":
							link = "https://elsewhere.invalid" + ciChecksPath + "?page=2&per_page=100&filter=all"
						}
						if mode != "missing_rows" {
							w.Header().Set("Link", "<"+link+">; rel=next")
						}
					} else {
						row = ciCheckFixture(2)
						if mode == "total_drift" {
							total = 3
						}
						if mode == "duplicate_check" || mode == "conflicting_check" {
							row["id"] = 1
						}
						if mode == "conflicting_check" {
							row["conclusion"] = "failure"
						}
					}
					rows := []map[string]any{row}
					if mode == "empty_more" {
						rows = []map[string]any{}
					}
					_ = json.NewEncoder(w).Encode(ciEnvelope(rows, total))
					return
				}
				if r.URL.Path != ciStatusesPath {
					t.Error("followed wrong scope", r.URL.String())
					http.NotFound(w, r)
					return
				}
				if mode == "partial_status" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"message":"PRIVATE_PROVIDER_DIAGNOSTIC"}`))
					return
				}
				row := ciStatusFixture(1)
				if page == "1" {
					w.Header().Set("Link", "<"+origin+ciStatusesPath+"?page=2&per_page=100>; rel=next")
				} else if mode == "null_second" {
					_, _ = w.Write([]byte(`null`))
					return
				} else if mode == "conflicting_status" {
					row["state"] = "failure"
				}
				_ = json.NewEncoder(w).Encode([]map[string]any{row})
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
			assertNoCIInventory(t, got, err)
			limit := int64(5)
			if mode == "total_drift" {
				limit = 6
				if !hasCode(err, "provider_observation_changed") {
					t.Fatal("movement lost its diagnostic", err)
				}
			}
			if requests.Load() > limit {
				t.Fatal("pagination retried", requests.Load())
			}
		})
	}
}

func TestCIResultsSelectorsBeforeIOAndPermissions(t *testing.T) {
	g, calls := ciFixtureServer(t, `{"total_count":0,"check_runs":[]}`, `[]`)
	for _, input := range [][2]string{{"../repo", ciHead}, {"owner/repo/extra", ciHead}, {"owner/repo", "main"}, {"owner/repo", strings.ToUpper(ciHead)}, {"owner/repo", ""}} {
		got, err := g.CIResults(t.Context(), input[0], input[1])
		assertNoCIInventory(t, got, err)
	}
	got, err := g.CIResults(nil, "owner/repo", ciHead)
	assertNoCIInventory(t, got, err)
	if calls.Load() != 0 {
		t.Fatal("invalid selector reached provider")
	}
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusMovedPermanently} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/PRIVATE_DIAGNOSTIC")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"message":"PRIVATE_DIAGNOSTIC"}`))
		}))
		got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
		server.Close()
		assertNoCIInventory(t, got, err)
	}
}

type ciRoundTripper func(*http.Request) (*http.Response, error)

func (f ciRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCIResultsSharedDeadlineCancellationAndRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ciSuitesPath {
			_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
			return
		}
		if r.URL.Path == ciChecksPath {
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	g := testGitHub(server, "")
	if g.httpClient.Timeout != 5*time.Second {
		t.Fatal("per-request timeout changed", g.httpClient.Timeout)
	}
	transport := g.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var deadlines []time.Time
	g.httpClient.Timeout = 0 // expose the outer aggregate deadline in the test transport
	g.httpClient.Transport = ciRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("missing aggregate deadline")
		}
		deadlines = append(deadlines, deadline)
		return transport.RoundTrip(r)
	})
	before := time.Now()
	got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
	if err != nil || len(deadlines) != 4 || !deadlines[0].Equal(deadlines[1]) || !deadlines[0].Equal(deadlines[2]) || !deadlines[0].Equal(deadlines[3]) || deadlines[0].Before(before.Add(14*time.Second)) || deadlines[0].After(time.Now().Add(15*time.Second)) || got.Checks == nil {
		t.Fatal("aggregate deadline restarted", deadlines, err)
	}

	entered, exited := make(chan struct{}), make(chan struct{})
	var count atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.URL.Path == ciSuitesPath {
			_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
			return
		}
		if r.URL.Path == ciChecksPath {
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			return
		}
		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	defer blocked.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		result, err := testGitHub(blocked, "").CIResults(ctx, "owner/repo", ciHead)
		if !reflect.DeepEqual(result, CIResultInventory{}) {
			done <- fmt.Errorf("partial result on cancel")
			return
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("status read not entered")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation unbounded")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("owned HTTP request not cancelled")
	}
	if count.Load() != 3 {
		t.Fatal("cancellation retried", count.Load())
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	got, err = testGitHub(blocked, "").CIResults(ctx, "owner/repo", ciHead)
	assertNoCIInventory(t, got, err)
	if count.Load() != 3 {
		t.Fatal("already cancelled call reached provider")
	}
}

func TestCIResultsSharedPageItemAndByteBudgets(t *testing.T) {
	t.Run("shared_pages", func(t *testing.T) {
		var origin string
		var count atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			if r.URL.Path == ciSuitesPath {
				_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			fixed := ""
			limit := 50
			if r.URL.Path == ciChecksPath {
				fixed = "&filter=all"
				limit = 51
			}
			if page < limit {
				w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100%s>; rel=next", origin, r.URL.Path, page+1, fixed))
			}
			if r.URL.Path == ciChecksPath {
				_ = json.NewEncoder(w).Encode(ciEnvelope([]map[string]any{ciCheckFixture(int64(page))}, 51))
			} else {
				_ = json.NewEncoder(w).Encode([]map[string]any{ciStatusFixture(int64(page))})
			}
		}))
		defer server.Close()
		origin = server.URL
		got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
		if count.Load() != 100 {
			t.Fatal("page budget not shared", count.Load())
		}
	})
	t.Run("page_size", func(t *testing.T) {
		rows := make([]map[string]any, 101)
		for i := range rows {
			rows[i] = ciStatusFixture(int64(i + 1))
		}
		g, _ := ciFixtureServer(t, `{"total_count":0,"check_runs":[]}`, ciJSON(rows))
		got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
	})
	t.Run("shared_item_boundary", func(t *testing.T) {
		g, _ := ciFixtureServer(t, `{"total_count":0,"check_runs":[]}`, ciJSON([]map[string]any{ciStatusFixture(1), ciStatusFixture(2)}))
		rows, err := g.ciStatuses(t.Context(), ciStatusesPath, &actionsBudget{items: 9999})
		if err == nil || rows != nil {
			t.Fatal("shared item budget ignored", rows, err)
		}
	})
	t.Run("shared_bytes", func(t *testing.T) {
		var origin string
		var count atomic.Int64
		padding := strings.Repeat("x", 6<<20)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			if r.URL.Path == ciSuitesPath {
				_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
				return
			}
			if r.URL.Path == ciChecksPath {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page == 1 {
					w.Header().Set("Link", "<"+origin+ciChecksPath+"?page=2&per_page=100&filter=all>; rel=next")
				}
				body := ciEnvelope([]map[string]any{ciCheckFixture(int64(page))}, 2)
				body["ignored_metadata"] = padding
				_ = json.NewEncoder(w).Encode(body)
			} else {
				row := ciStatusFixture(1)
				row["ignored_metadata"] = padding
				_ = json.NewEncoder(w).Encode([]map[string]any{row})
			}
		}))
		defer server.Close()
		origin = server.URL
		got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
		if count.Load() != 4 {
			t.Fatal("aggregate limit not shared", count.Load())
		}
	})
	t.Run("response_bytes", func(t *testing.T) {
		g, _ := ciFixtureServer(t, `{"total_count":0,"check_runs":[],"metadata":"`+strings.Repeat("x", githubMaxBody)+`"}`, `[]`)
		got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
		assertNoCIInventory(t, got, err)
	})
}

// Ensure a URL-encoded selector cannot replace the literal immutable SHA.
func TestCIResultsPaginationRejectsEncodedHeadAlias(t *testing.T) {
	var origin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ciSuitesPath {
			_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(1))
			return
		}
		path := strings.Replace(ciChecksPath, ciHead, "%31"+ciHead[1:], 1)
		w.Header().Set("Link", "<"+origin+path+"?"+url.Values{"page": {"2"}, "per_page": {"100"}, "filter": {"all"}}.Encode()+">; rel=next")
		_ = json.NewEncoder(w).Encode(ciEnvelope([]map[string]any{ciCheckFixture(1)}, 2))
	}))
	defer server.Close()
	origin = server.URL
	got, err := testGitHub(server, "").CIResults(t.Context(), "owner/repo", ciHead)
	assertNoCIInventory(t, got, err)
}
