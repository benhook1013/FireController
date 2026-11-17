package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestActionsLivenessBoundedReadOnlyInventory(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != headRunsPath || len(r.URL.Query()) != 3 || r.URL.Query().Get("head_sha") != "" {
			t.Error("unexpected mutation/filter", r.Method, r.URL)
		}
		rows := []map[string]any{}
		if r.URL.Query().Get("status") == "queued" {
			x := headRunFixture(1)
			x["status"] = "queued"
			x["conclusion"] = nil
			rows = append(rows, x)
		}
		json.NewEncoder(w).Encode(headRunEnvelope(rows, len(rows)))
	}))
	defer s.Close()
	got, err := testGitHub(s, "test").ObserveActionsLiveness(t.Context(), "owner/repo", 20, time.Hour)
	if err != nil || calls != 5 || len(got.Runs) != 1 || !got.Runs[0].Aged || got.Runs[0].Capacity != "unknown" || got.Runs[0].HeadSHA != headRunsSHA {
		t.Fatal(got, err, calls)
	}
}

func TestActionsLivenessRefusesPartialOrMovingInventory(t *testing.T) {
	for _, mode := range []string{"overflow", "missing", "wrong_repo", "completed", "future", "duplicate", "provider_failure", "link_escape"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := r.URL.Query().Get("status")
				rows := []map[string]any{}
				total := 0
				if status == "queued" || mode == "duplicate" && status == "in_progress" {
					x := headRunFixture(1)
					x["status"] = status
					x["conclusion"] = nil
					rows = append(rows, x)
					total = 1
					switch mode {
					case "overflow":
						total = 201
					case "missing":
						total = 2
					case "wrong_repo":
						x["repository"] = map[string]any{"id": 99, "full_name": "owner/repo"}
					case "completed":
						x["status"] = "completed"
						x["conclusion"] = "success"
					case "future":
						x["updated_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
					case "link_escape":
						total = 2
						w.Header().Set("Link", `<https://evil.invalid/repos/owner/repo/actions/runs?page=2&status=queued&per_page=100>; rel="next"`)
					}
				}
				if mode == "provider_failure" && status == "in_progress" {
					w.WriteHeader(503)
					return
				}
				json.NewEncoder(w).Encode(headRunEnvelope(rows, total))
			}))
			defer s.Close()
			got, err := testGitHub(s, "test").ObserveActionsLiveness(t.Context(), "owner/repo", 20, time.Hour)
			if err == nil || !reflect.DeepEqual(got, ActionsLiveness{}) {
				t.Fatal("partial facts survived", got, err)
			}
		})
	}
}

func TestActionsLivenessInvalidSelectorNoRequests(t *testing.T) {
	g, calls := headRunServer(t, headRunJSON(headRunEnvelope([]map[string]any{}, 0)))
	for _, age := range []time.Duration{0, time.Minute, 31 * 24 * time.Hour} {
		if _, err := g.ObserveActionsLiveness(t.Context(), "owner/repo", 20, age); err == nil {
			t.Fatal("age accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selector reached provider")
	}
}

func livenessRun(id int64, status string) map[string]any {
	row := headRunFixture(id)
	row["status"], row["conclusion"] = status, nil
	return row
}

func TestActionsLivenessRejectsContradictoryImmutableWorkflowIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		rows := []map[string]any{}
		if status == "queued" || status == "in_progress" {
			id := int64(1)
			if status == "in_progress" {
				id = 2
			}
			row := livenessRun(id, status)
			row["run_number"] = 1
			rows = append(rows, row)
		}
		_ = json.NewEncoder(w).Encode(headRunEnvelope(rows, len(rows)))
	}))
	defer server.Close()
	got, err := testGitHub(server, "").ObserveActionsLiveness(t.Context(), "owner/repo", 20, time.Hour)
	if err == nil || !reflect.DeepEqual(got, ActionsLiveness{}) {
		t.Fatal("contradictory identity survived", got, err)
	}
}

func TestActionsLivenessMaximumAllStatusInventoryCostsTenGETs(t *testing.T) {
	statuses := map[string]int64{"queued": 0, "in_progress": 1, "waiting": 2, "pending": 3, "requested": 4}
	var origin string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		status := r.URL.Query().Get("status")
		bucket, valid := statuses[status]
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if !valid || r.Method != http.MethodGet || r.URL.Path != headRunsPath || len(r.URL.Query()) != 3 || r.URL.Query().Get("per_page") != "100" || page < 1 || page > 2 {
			t.Error("unexpected request", r.Method, r.URL)
		}
		rows := make([]map[string]any, 0, 100)
		for i := 1; i <= 100; i++ {
			rows = append(rows, livenessRun(bucket*200+int64((page-1)*100+i), status))
		}
		if page == 1 {
			w.Header().Set("Link", fmt.Sprintf("<%s%s?page=2&per_page=100&status=%s>; rel=next", origin, headRunsPath, status))
		}
		_ = json.NewEncoder(w).Encode(headRunEnvelope(rows, 200))
	}))
	defer server.Close()
	origin = server.URL
	got, err := testGitHub(server, "").ObserveActionsLiveness(t.Context(), "owner/repo", 20, time.Hour)
	if err != nil || calls != 10 || len(got.Runs) != 1000 || got.ObservedAt.IsZero() {
		t.Fatal("bounded complete scan failed", calls, len(got.Runs), err)
	}
	for _, run := range got.Runs {
		if !run.Aged || run.Capacity != "unknown" || run.Conclusion != "" || run.RunAttempt != 2 {
			t.Fatal("inferred authority or lost identity", run)
		}
	}
}

func TestActionsLivenessAllowsSameNumberForDistinctWorkflows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{}
		if r.URL.Query().Get("status") == "queued" {
			first, second := livenessRun(1, "queued"), livenessRun(2, "queued")
			second["workflow_id"], second["run_number"] = 31, 1
			rows = append(rows, first, second)
		}
		_ = json.NewEncoder(w).Encode(headRunEnvelope(rows, len(rows)))
	}))
	defer server.Close()
	got, err := testGitHub(server, "").ObserveActionsLiveness(t.Context(), "owner/repo", 20, time.Hour)
	if err != nil || len(got.Runs) != 2 {
		t.Fatal("distinct workflows conflated", got, err)
	}
}

func TestActionsLivenessRefusesIncompletePaginationWithoutPartialFacts(t *testing.T) {
	for _, mode := range []string{"changed_total", "missing_last", "duplicate_page", "changed_status_link", "extra_filter_link", "empty_more", "third_page"} {
		t.Run(mode, func(t *testing.T) {
			var origin string
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				status := r.URL.Query().Get("status")
				if status != "queued" {
					t.Error("partial scan continued", status)
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				rows := []map[string]any{livenessRun(int64(page), status)}
				total := 2
				if page == 1 {
					link := fmt.Sprintf("<%s%s?page=2&per_page=100&status=queued>; rel=next", origin, headRunsPath)
					switch mode {
					case "missing_last":
						link = ""
					case "changed_status_link":
						link = fmt.Sprintf("<%s%s?page=2&per_page=100&status=pending>; rel=next", origin, headRunsPath)
					case "extra_filter_link":
						link = fmt.Sprintf("<%s%s?page=2&per_page=100&status=queued&head_sha=%s>; rel=next", origin, headRunsPath, headRunsSHA)
					case "empty_more":
						rows = []map[string]any{}
					}
					if link != "" {
						w.Header().Set("Link", link)
					}
				} else {
					switch mode {
					case "changed_total":
						total = 3
					case "duplicate_page":
						rows = []map[string]any{livenessRun(1, status)}
					case "third_page":
						total = 2
						w.Header().Set("Link", fmt.Sprintf("<%s%s?page=3&per_page=100&status=queued>; rel=next", origin, headRunsPath))
					}
				}
				_ = json.NewEncoder(w).Encode(headRunEnvelope(rows, total))
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").ObserveActionsLiveness(t.Context(), "owner/repo", 20, time.Hour)
			if err == nil || !reflect.DeepEqual(got, ActionsLiveness{}) || calls > 2 {
				t.Fatal("incomplete scan credited or request bound exceeded", got, err, calls)
			}
		})
	}
}

func TestActionsLivenessUsesOneDeadlineAcrossStatusesAndCooperativeCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(headRunEnvelope([]map[string]any{}, 0))
	}))
	defer server.Close()
	g := testGitHub(server, "")
	if g.httpClient.Timeout != 5*time.Second {
		t.Fatal("ordinary request timeout changed")
	}
	base := g.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	// Observe the collector's aggregate deadline independently of the existing request timeout.
	g.httpClient.Timeout = 0
	var deadlines []time.Time
	g.httpClient.Transport = headRunTransport(func(r *http.Request) (*http.Response, error) {
		d, ok := r.Context().Deadline()
		if !ok {
			t.Error("aggregate deadline absent")
		}
		deadlines = append(deadlines, d)
		return base.RoundTrip(r)
	})
	before := time.Now()
	got, err := g.ObserveActionsLiveness(t.Context(), "owner/repo", 20, time.Hour)
	if err != nil || len(got.Runs) != 0 || len(deadlines) != 5 {
		t.Fatal(got, err, deadlines)
	}
	for _, d := range deadlines {
		if !d.Equal(deadlines[0]) || d.Before(before.Add(14*time.Second)) || d.After(time.Now().Add(15*time.Second)) {
			t.Fatal("deadline restarted", deadlines)
		}
	}

	entered, exited := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("status") == "queued" {
			_ = json.NewEncoder(w).Encode(headRunEnvelope([]map[string]any{livenessRun(1, "queued")}, 1))
			return
		}
		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	defer blocked.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		facts ActionsLiveness
		err   error
	}
	done := make(chan result, 1)
	go func() {
		facts, err := testGitHub(blocked, "").ObserveActionsLiveness(ctx, "owner/repo", 20, time.Hour)
		done <- result{facts, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second status not entered")
	}
	cancel()
	select {
	case a := <-done:
		if a.err == nil || !reflect.DeepEqual(a.facts, ActionsLiveness{}) {
			t.Fatal("partial facts after cancellation", a)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation unbounded")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("owned request still open")
	}
	if calls.Load() != 2 {
		t.Fatal("cancellation retried", calls.Load())
	}
}

func TestActionsLivenessCancelledCallerAndInvalidIdentityMakeNoRequests(t *testing.T) {
	g, calls := headRunServer(t, headRunJSON(headRunEnvelope([]map[string]any{}, 0)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	selectors := []struct {
		ctx  context.Context
		repo string
		id   int64
	}{{nil, "owner/repo", 20}, {t.Context(), "bad/repo/extra", 20}, {t.Context(), "owner/repo", 0}, {ctx, "owner/repo", 20}}
	for _, s := range selectors {
		if got, err := g.ObserveActionsLiveness(s.ctx, s.repo, s.id, time.Hour); err == nil || !reflect.DeepEqual(got, ActionsLiveness{}) {
			t.Fatal("invalid observation accepted", got, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selector/cancelled caller reached provider", calls.Load())
	}
}
