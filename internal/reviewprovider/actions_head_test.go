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

const headRunsSHA = "abcdef0123456789abcdef0123456789abcdef01"
const headRunsPath = "/repos/owner/repo/actions/runs"

func headRunFixture(id int64) map[string]any {
	return map[string]any{"id": id, "check_suite_id": id + 1000, "workflow_id": 30, "run_number": id, "run_attempt": 2, "head_sha": headRunsSHA, "event": "pull_request", "status": "completed", "conclusion": "success", "created_at": "2026-01-01T00:00:01+00:00", "updated_at": "2026-01-01T00:00:02.123Z", "repository": map[string]any{"id": 20, "full_name": "Owner/Repo"}, "head_repository": map[string]any{"id": 999, "full_name": "fork/repo"}, "logs_url": "PRIVATE_LOG_URL", "actor": map[string]any{"login": "PRIVATE_ACTOR"}}
}

func headRunEnvelope(rows []map[string]any, total int) map[string]any {
	return map[string]any{"total_count": total, "workflow_runs": rows}
}

func headRunJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func headRunServer(t *testing.T, body string) (*GitHub, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != headRunsPath {
			t.Error("unrelated method/path", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return testGitHub(server, "synthetic-head-token"), calls
}

func assertNoHeadRuns(t *testing.T, inventory ActionsHeadInventory, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(inventory, ActionsHeadInventory{}) || strings.Contains(err.Error(), "PRIVATE_") || strings.Contains(err.Error(), "synthetic-head-token") {
		t.Fatal("failed inventory returned partial facts/unsafe error", inventory, err)
	}
}

func TestActionsRunsForHeadRetainsNewSuitesPendingRunsAndOtherWorkflowEvents(t *testing.T) {
	var origin string
	var paths []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != headRunsPath || len(q) != 3 || q.Get("head_sha") != headRunsSHA || q.Get("per_page") != "100" || r.Header.Get("Authorization") != "Bearer synthetic-head-token" {
			t.Error("selected head/method changed", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		var rows []map[string]any
		switch q.Get("page") {
		case "1":
			rows = []map[string]any{headRunFixture(1)}
			w.Header().Set("Link", "<"+origin+headRunsPath+"?page=2&per_page=100&head_sha="+headRunsSHA+">; rel=next")
		case "2":
			pending := headRunFixture(2)
			pending["status"], pending["conclusion"], pending["run_attempt"] = "queued", nil, 1
			manual := headRunFixture(3)
			manual["workflow_id"], manual["run_number"], manual["event"], manual["conclusion"] = 31, 1, "workflow_dispatch", "failure"
			rows = []map[string]any{pending, manual}
		default:
			t.Error("unexpected page", q)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(headRunEnvelope(rows, 3))
	}))
	defer server.Close()
	origin = server.URL
	before := time.Now()
	got, err := testGitHub(server, "synthetic-head-token").ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
	if err != nil || got.Repository != "owner/repo" || got.RepositoryID != 20 || got.HeadSHA != headRunsSHA || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now()) || got.ObservedAt.Location() != time.UTC || len(got.Runs) != 3 {
		t.Fatal("selected inventory missing", got, err)
	}
	if got.Runs[0].ID != 1 || got.Runs[0].RunNumber != 1 || got.Runs[0].RunAttempt != 2 || got.Runs[1].ID != 2 || got.Runs[1].Status != "queued" || got.Runs[1].Conclusion != "" || got.Runs[1].CheckSuiteID == got.Runs[0].CheckSuiteID || got.Runs[2].Event != "workflow_dispatch" || got.Runs[2].Conclusion != "failure" || got.Runs[2].WorkflowID == got.Runs[0].WorkflowID || got.Runs[2].RunNumber != 1 || got.Runs[0].CreatedAt.Location() != time.UTC || got.Runs[0].Repository != "Owner/Repo" {
		t.Fatal("inventory inferred a latest/applicable/passing run", got)
	}
	for _, private := range []string{"PRIVATE_LOG_URL", "PRIVATE_ACTOR", "fork/repo", "latest", "ready", "synthetic-head-token"} {
		if strings.Contains(headRunJSON(got), private) {
			t.Fatal("DTO expanded unrelated facts", private)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatal("discovered/queried additional resources", paths)
	}
}

func TestActionsRunsForHeadEmptyAndEveryLifecycleRemainInventories(t *testing.T) {
	g, calls := headRunServer(t, `{"total_count":0,"workflow_runs":[]}`)
	got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
	if err != nil || got.Runs == nil || len(got.Runs) != 0 || calls.Load() != 1 {
		t.Fatal("empty inventory not proven", got, err)
	}
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested", "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure"} {
		t.Run(status, func(t *testing.T) {
			row := headRunFixture(1)
			switch status {
			case "queued", "in_progress", "waiting", "pending", "requested":
				row["status"], row["conclusion"] = status, nil
			default:
				row["conclusion"] = status
			}
			g, _ := headRunServer(t, headRunJSON(headRunEnvelope([]map[string]any{row}, 1)))
			got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
			if err != nil || len(got.Runs) != 1 || got.Runs[0].Status != row["status"] {
				t.Fatal("lifecycle fact rejected", got, err)
			}
		})
	}
}

func TestActionsRunsForHeadRequiredFieldsAndBindingsFailClosed(t *testing.T) {
	for _, field := range []string{"id", "check_suite_id", "workflow_id", "run_number", "run_attempt", "head_sha", "event", "status", "conclusion", "created_at", "updated_at", "repository"} {
		for _, mode := range []string{"missing", "null", "wrongtype", "case"} {
			if field == "conclusion" && mode == "null" {
				continue
			} // completed/null has a separate lifecycle case below
			t.Run(field+"_"+mode, func(t *testing.T) {
				row := headRunFixture(1)
				switch mode {
				case "missing":
					delete(row, field)
				case "null":
					row[field] = nil
				case "wrongtype":
					row[field] = false
				case "case":
					value := row[field]
					delete(row, field)
					row[strings.ToUpper(field)] = value
				}
				g, calls := headRunServer(t, headRunJSON(headRunEnvelope([]map[string]any{row}, 1)))
				got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
				assertNoHeadRuns(t, got, err)
				if calls.Load() != 1 {
					t.Fatal("invalid inventory retried")
				}
			})
		}
	}
	cases := map[string]func(map[string]any){
		"id_zero":                 func(r map[string]any) { r["id"] = 0 },
		"id_fractional":           func(r map[string]any) { r["id"] = 1.5 },
		"number_zero":             func(r map[string]any) { r["run_number"] = 0 },
		"attempt_zero":            func(r map[string]any) { r["run_attempt"] = 0 },
		"workflow_negative":       func(r map[string]any) { r["workflow_id"] = -1 },
		"suite_zero":              func(r map[string]any) { r["check_suite_id"] = 0 },
		"head_movement":           func(r map[string]any) { r["head_sha"] = strings.Repeat("f", 40) },
		"upper_head":              func(r map[string]any) { r["head_sha"] = strings.ToUpper(headRunsSHA) },
		"repo_id":                 func(r map[string]any) { r["repository"] = map[string]any{"id": 21, "full_name": "owner/repo"} },
		"repo_name":               func(r map[string]any) { r["repository"] = map[string]any{"id": 20, "full_name": "other/repo"} },
		"repo_missing_id":         func(r map[string]any) { r["repository"] = map[string]any{"full_name": "owner/repo"} },
		"repo_null_name":          func(r map[string]any) { r["repository"] = map[string]any{"id": 20, "full_name": nil} },
		"event_empty":             func(r map[string]any) { r["event"] = "" },
		"event_control":           func(r map[string]any) { r["event"] = "pull_request\n" },
		"status_unknown":          func(r map[string]any) { r["status"] = "future" },
		"conclusion_unknown":      func(r map[string]any) { r["conclusion"] = "passed" },
		"completed_no_conclusion": func(r map[string]any) { r["conclusion"] = nil },
		"queued_conclusion":       func(r map[string]any) { r["status"] = "queued" },
		"time_invalid":            func(r map[string]any) { r["created_at"] = "yesterday" },
		"time_zero":               func(r map[string]any) { r["created_at"] = "0001-01-01T00:00:00Z" },
		"time_reversed":           func(r map[string]any) { r["updated_at"] = "2025-01-01T00:00:00Z" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			row := headRunFixture(1)
			mutate(row)
			g, _ := headRunServer(t, headRunJSON(headRunEnvelope([]map[string]any{row}, 1)))
			got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
			assertNoHeadRuns(t, got, err)
		})
	}
	for _, body := range []string{`null`, `{}`, `{"total_count":0}`, `{"workflow_runs":[]}`, `{"total_count":0,"workflow_runs":null}`, `{"total_count":1,"workflow_runs":[null]}`, `{"total_count":-1,"workflow_runs":[]}`, `{"total_count":1.0,"workflow_runs":[]}`, `{"total_count":0,"total_count":0,"workflow_runs":[]}`, `{"total_count":1,"workflow_runs":[{"id":1,"id":2}]}`, `{"total_count":1,"workflow_runs":[{"id":1,"event":"\ud800"}]}`, "{\"total_count\":0,\"workflow_runs\":[],\"other\":\"\xff\"}"} {
		g, _ := headRunServer(t, body)
		got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
		assertNoHeadRuns(t, got, err)
	}
}

func TestActionsRunsForHeadPaginationTotalAndWorkflowNumberConflicts(t *testing.T) {
	for _, mode := range []string{"total_drift", "missing_rows", "empty_next", "duplicate_id", "conflicting_id", "workflow_number", "missing_head", "changed_head", "suite_filter", "extra_event", "duplicate_head", "loop", "foreign", "encoded_path", "null_second", "partial_forbidden"} {
		t.Run(mode, func(t *testing.T) {
			var origin string
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != headRunsPath {
					t.Error("followed out-of-scope resource", r.URL.String())
					http.NotFound(w, r)
					return
				}
				page := r.URL.Query().Get("page")
				row := headRunFixture(1)
				total := 2
				if page == "1" {
					link := origin + headRunsPath + "?page=2&per_page=100&head_sha=" + headRunsSHA
					switch mode {
					case "missing_head":
						link = origin + headRunsPath + "?page=2&per_page=100"
					case "changed_head":
						link = strings.Replace(link, headRunsSHA, strings.Repeat("f", 40), 1)
					case "suite_filter":
						link += "&check_suite_id=1001"
					case "extra_event":
						link += "&event=pull_request"
					case "duplicate_head":
						link += "&head_sha=" + headRunsSHA
					case "loop":
						link = strings.Replace(link, "page=2", "page=1", 1)
					case "foreign":
						link = "https://elsewhere.invalid" + headRunsPath + "?page=2&per_page=100&head_sha=" + headRunsSHA
					case "encoded_path":
						link = strings.Replace(link, "/runs?", "/%72uns?", 1)
					}
					if mode != "missing_rows" {
						w.Header().Set("Link", "<"+link+">; rel=next")
					}
				} else {
					if mode == "null_second" {
						_, _ = w.Write([]byte(`null`))
						return
					}
					if mode == "partial_forbidden" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"message":"PRIVATE_DIAGNOSTIC"}`))
						return
					}
					row = headRunFixture(2)
					switch mode {
					case "total_drift":
						total = 3
					case "duplicate_id":
						row = headRunFixture(1)
					case "conflicting_id":
						row["id"] = 1
					case "workflow_number":
						row["run_number"] = 1
					}
					if mode == "conflicting_id" {
						row["workflow_id"] = 31
					}
				}
				rows := []map[string]any{row}
				if mode == "empty_next" {
					rows = []map[string]any{}
				}
				_ = json.NewEncoder(w).Encode(headRunEnvelope(rows, total))
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
			assertNoHeadRuns(t, got, err)
			if calls.Load() > 2 {
				t.Fatal("failure retried or followed unrelated scope", calls.Load())
			}
		})
	}
}

func TestActionsRunsForHeadFilteredProviderCapAnd999Boundary(t *testing.T) {
	for _, total := range []int{999, 1000, 1001, 10001} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			var origin string
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				start := (page - 1) * 100
				end := min(start+100, total)
				rows := make([]map[string]any, 0, end-start)
				for i := start; i < end; i++ {
					rows = append(rows, headRunFixture(int64(i+1)))
				}
				if end < total {
					w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100&head_sha=%s>; rel=next", origin, headRunsPath, page+1, headRunsSHA))
				}
				_ = json.NewEncoder(w).Encode(headRunEnvelope(rows, total))
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
			if total == 999 {
				if err != nil || len(got.Runs) != 999 || calls.Load() != 10 {
					t.Fatal("999 boundary not complete", len(got.Runs), err, calls.Load())
				}
			} else {
				assertNoHeadRuns(t, got, err)
				if calls.Load() != 1 {
					t.Fatal("truncated result paged or accepted", calls.Load())
				}
			}
		})
	}
}

func TestActionsRunsForHeadInvalidSelectorsNoIOAndPermissions(t *testing.T) {
	g, calls := headRunServer(t, `{"total_count":0,"workflow_runs":[]}`)
	for _, input := range []struct {
		repo string
		id   int64
		head string
	}{{"owner/repo", 0, headRunsSHA}, {"owner/repo", -1, headRunsSHA}, {"../repo", 20, headRunsSHA}, {"owner/repo/extra", 20, headRunsSHA}, {"owner/repo", 20, "main"}, {"owner/repo", 20, strings.ToUpper(headRunsSHA)}, {"owner/repo", 20, ""}} {
		got, err := g.ActionsRunsForHead(t.Context(), input.repo, input.id, input.head)
		assertNoHeadRuns(t, got, err)
	}
	got, err := g.ActionsRunsForHead(nil, "owner/repo", 20, headRunsSHA)
	assertNoHeadRuns(t, got, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = g.ActionsRunsForHead(ctx, "owner/repo", 20, headRunsSHA)
	assertNoHeadRuns(t, got, err)
	if calls.Load() != 0 {
		t.Fatal("invalid/cancelled invocation reached provider", calls.Load())
	}
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusMovedPermanently} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/PRIVATE_REDIRECT")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"PRIVATE_DIAGNOSTIC"}`))
		}))
		got, err := testGitHub(server, "").ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
		server.Close()
		assertNoHeadRuns(t, got, err)
	}
}

type headRunTransport func(*http.Request) (*http.Response, error)

func (f headRunTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestActionsRunsForHeadSharedDeadlineAndCooperativeCancellation(t *testing.T) {
	var origin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 1 {
			w.Header().Set("Link", "<"+origin+headRunsPath+"?page=2&per_page=100&head_sha="+headRunsSHA+">; rel=next")
		}
		_ = json.NewEncoder(w).Encode(headRunEnvelope([]map[string]any{headRunFixture(int64(page))}, 2))
	}))
	defer server.Close()
	origin = server.URL
	g := testGitHub(server, "")
	if g.httpClient.Timeout != 5*time.Second {
		t.Fatal("request budget changed")
	}
	base := g.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	g.httpClient.Timeout = 0
	var deadlines []time.Time
	g.httpClient.Transport = headRunTransport(func(r *http.Request) (*http.Response, error) {
		d, ok := r.Context().Deadline()
		if !ok {
			t.Error("missing aggregate deadline")
		}
		deadlines = append(deadlines, d)
		return base.RoundTrip(r)
	})
	before := time.Now()
	got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
	if err != nil || len(got.Runs) != 2 || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) || deadlines[0].Before(before.Add(14*time.Second)) || deadlines[0].After(time.Now().Add(15*time.Second)) {
		t.Fatal("aggregate budget restarted", deadlines, err)
	}

	entered, exited := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	var blockedOrigin string
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", "<"+blockedOrigin+headRunsPath+"?page=2&per_page=100&head_sha="+headRunsSHA+">; rel=next")
			_ = json.NewEncoder(w).Encode(headRunEnvelope([]map[string]any{headRunFixture(1)}, 2))
			return
		}
		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	defer blocked.Close()
	blockedOrigin = blocked.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type answer struct {
		result ActionsHeadInventory
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, err := testGitHub(blocked, "").ActionsRunsForHead(ctx, "owner/repo", 20, headRunsSHA)
		done <- answer{result, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second page not entered")
	}
	cancel()
	select {
	case a := <-done:
		assertNoHeadRuns(t, a.result, a.err)
	case <-time.After(time.Second):
		t.Fatal("cancellation unbounded")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("owned request remained open")
	}
	if calls.Load() != 2 {
		t.Fatal("cancellation retried", calls.Load())
	}
}

func TestActionsRunsForHeadCollectionBudgets(t *testing.T) {
	t.Run("pages", func(t *testing.T) {
		var origin string
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100&head_sha=%s>; rel=next", origin, headRunsPath, page+1, headRunsSHA))
			_ = json.NewEncoder(w).Encode(headRunEnvelope([]map[string]any{headRunFixture(int64(page))}, 101))
		}))
		defer server.Close()
		origin = server.URL
		got, err := testGitHub(server, "").ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
		assertNoHeadRuns(t, got, err)
		if calls.Load() != 100 {
			t.Fatal("page cap not enforced", calls.Load())
		}
	})
	t.Run("page_size", func(t *testing.T) {
		rows := make([]map[string]any, 101)
		for i := range rows {
			rows[i] = headRunFixture(int64(i + 1))
		}
		g, _ := headRunServer(t, headRunJSON(headRunEnvelope(rows, 101)))
		got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
		assertNoHeadRuns(t, got, err)
	})
	t.Run("shared_items", func(t *testing.T) {
		g, _ := headRunServer(t, headRunJSON(headRunEnvelope([]map[string]any{headRunFixture(1), headRunFixture(2)}, 2)))
		rows, err := g.actionsHeadRuns(t.Context(), headRunsPath, "owner/repo", 20, headRunsSHA, &actionsBudget{items: 9999})
		if err == nil || rows != nil {
			t.Fatal("shared item bound ignored", rows, err)
		}
	})
	t.Run("aggregate_bytes", func(t *testing.T) {
		var origin string
		var calls atomic.Int64
		padding := strings.Repeat("x", 6<<20)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 3 {
				w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100&head_sha=%s>; rel=next", origin, headRunsPath, page+1, headRunsSHA))
			}
			body := headRunEnvelope([]map[string]any{headRunFixture(int64(page))}, 3)
			body["ignored_metadata"] = padding
			_ = json.NewEncoder(w).Encode(body)
		}))
		defer server.Close()
		origin = server.URL
		got, err := testGitHub(server, "").ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
		assertNoHeadRuns(t, got, err)
		if calls.Load() != 3 {
			t.Fatal("aggregate bytes not bounded", calls.Load())
		}
	})
	t.Run("response_bytes", func(t *testing.T) {
		g, _ := headRunServer(t, `{"total_count":0,"workflow_runs":[],"ignored":"`+strings.Repeat("x", githubMaxBody)+`"}`)
		got, err := g.ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
		assertNoHeadRuns(t, got, err)
	})
	t.Run("seeded_page_budget", func(t *testing.T) {
		g, calls := headRunServer(t, `{"total_count":0,"workflow_runs":[]}`)
		_, err := g.actionsHeadRuns(t.Context(), headRunsPath, "owner/repo", 20, headRunsSHA, &actionsBudget{pages: 100})
		if err == nil || calls.Load() != 0 {
			t.Fatal("exhausted page budget reached provider")
		}
	})
}

// Pagination must never introduce a job, suite, event, or status selector.
func TestActionsRunsForHeadQueryIsExactlyHeadOnly(t *testing.T) {
	var seen atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		want := url.Values{"head_sha": {headRunsSHA}, "page": {"1"}, "per_page": {"100"}}
		if !reflect.DeepEqual(r.URL.Query(), want) {
			t.Error("query contains extra authority", r.URL.Query())
		}
		_ = json.NewEncoder(w).Encode(headRunEnvelope([]map[string]any{}, 0))
	}))
	defer server.Close()
	got, err := testGitHub(server, "").ActionsRunsForHead(t.Context(), "owner/repo", 20, headRunsSHA)
	if err != nil || got.Runs == nil || seen.Load() != 1 {
		t.Fatal("head-only empty query failed", got, err)
	}
}
