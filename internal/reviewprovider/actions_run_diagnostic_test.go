package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestObserveActionsRunAttemptExactTenGETBoundaryAndSafeFacts(t *testing.T) {
	var calls, metadata atomic.Int64
	var origin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-secret" {
			t.Error("not exact authenticated read")
		}
		switch r.URL.Path {
		case actionsCurrentPath:
			if r.URL.RawQuery != "" {
				t.Error("unexpected metadata selector")
			}
			run := actionsRunFixture()
			if metadata.Add(1) == 2 {
				run["updated_at"] = "2026-01-01T00:02:00Z"
			}
			_ = json.NewEncoder(w).Encode(run)
		case actionsJobsPath:
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 1 || page > 8 || r.URL.Query().Get("per_page") != "100" || len(r.URL.Query()) != 2 {
				t.Error("job selector escaped", r.URL.String())
			}
			rows := []map[string]any{}
			for i := 0; i < 100; i++ {
				rows = append(rows, actionsJobFixture(int64(page*100+i)))
			}
			if page < 8 {
				w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100>; rel=next", origin, actionsJobsPath, page+1))
			}
			_ = json.NewEncoder(w).Encode(actionsEnvelope("jobs", rows, 800))
		default:
			t.Error("unexpected discovery or mutation", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	origin = server.URL
	before := time.Now()
	got, err := testGitHub(server, "synthetic-secret").ObserveActionsRunAttempt(t.Context(), "owner/repo", 20, 50, 2)
	if err != nil || calls.Load() != 10 || metadata.Load() != 2 || len(got.Jobs) != 800 || got.Run.ID != 50 || got.Run.RepositoryID != 20 || got.Run.RunAttempt != 2 || got.Run.RunNumber != 7 || got.Run.WorkflowID != 30 || got.Run.CheckSuiteID != 40 || got.Run.HeadSHA != actionsHead || got.Run.UpdatedAt.Format(time.RFC3339) != "2026-01-01T00:02:00Z" || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now()) {
		t.Fatal(got, err, calls.Load())
	}
	raw, _ := json.Marshal(got)
	for _, private := range []string{"PRIVATE_LOG_URL", "PRIVATE_RUNNER", "synthetic-secret", "head_repository"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private data leaked", private)
		}
	}
}

func TestObserveActionsRunAttemptRefusesChangedOrInvalidIdentity(t *testing.T) {
	for _, phase := range []int64{1, 2} {
		for name, change := range map[string]func(map[string]any){
			"run": func(r map[string]any) { r["id"] = 51 }, "repository ID": func(r map[string]any) { r["repository"] = map[string]any{"id": 21, "full_name": "owner/repo"} }, "repository name": func(r map[string]any) { r["repository"] = map[string]any{"id": 20, "full_name": "other/repo"} }, "attempt": func(r map[string]any) { r["run_attempt"] = 3 }, "workflow": func(r map[string]any) { r["workflow_id"] = 31 }, "run number": func(r map[string]any) { r["run_number"] = 8 }, "suite": func(r map[string]any) { r["check_suite_id"] = 41 }, "head": func(r map[string]any) { r["head_sha"] = strings.Repeat("b", 40) }, "event": func(r map[string]any) { r["event"] = "push" }, "created": func(r map[string]any) { r["created_at"] = "2025-12-31T23:59:00Z" }, "status": func(r map[string]any) { r["status"] = "in_progress"; r["conclusion"] = nil }, "conclusion": func(r map[string]any) { r["conclusion"] = "failure" }, "invalid state": func(r map[string]any) { r["status"] = "unknown" }, "missing field": func(r map[string]any) { delete(r, "run_number") },
		} {
			if phase == 1 && name != "run" && name != "repository ID" && name != "repository name" && name != "attempt" && name != "invalid state" && name != "missing field" {
				continue
			}
			t.Run(fmt.Sprintf("%d/%s", phase, name), func(t *testing.T) {
				var metadata atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == actionsCurrentPath {
						run := actionsRunFixture()
						if metadata.Add(1) == phase {
							change(run)
						}
						_ = json.NewEncoder(w).Encode(run)
						return
					}
					if r.URL.Path != actionsJobsPath {
						t.Error("unexpected path", r.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(actionsEnvelope("jobs", []map[string]any{}, 0))
				}))
				defer server.Close()
				got, err := testGitHub(server, "").ObserveActionsRunAttempt(t.Context(), "owner/repo", 20, 50, 2)
				if err == nil || !reflect.DeepEqual(got, ActionsRunObservation{}) {
					t.Fatal("mismatch credited", got, err)
				}
			})
		}
	}
}

func TestObserveActionsRunAttemptJobBoundsAndMalformedRows(t *testing.T) {
	for _, mode := range []string{"801 total", "ninth page", "partial", "moving total", "duplicate jobs", "duplicate checks", "wrong run", "wrong attempt", "foreign URL", "null jobs"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			var origin string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == actionsCurrentPath {
					_ = json.NewEncoder(w).Encode(actionsRunFixture())
					return
				}
				if r.URL.Path != actionsJobsPath {
					t.Error("unexpected path", r.URL.Path)
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				rows := []map[string]any{actionsJobFixture(int64(page))}
				total := 1
				switch mode {
				case "801 total":
					total = 801
				case "ninth page":
					total = 9
					w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100>; rel=next", origin, actionsJobsPath, page+1))
					if page > 8 {
						t.Error("ninth request escaped bound")
					}
				case "partial":
					total = 2
				case "moving total":
					total = 2
					if page == 1 {
						w.Header().Set("Link", originLink(origin, 2))
					} else {
						total = 3
					}
				case "duplicate jobs":
					rows = append(rows, actionsJobFixture(int64(page)))
					total = 2
				case "duplicate checks":
					second := actionsJobFixture(2)
					second["check_run_url"] = rows[0]["check_run_url"]
					rows = append(rows, second)
					total = 2
				case "wrong run":
					rows[0]["run_id"] = 51
				case "wrong attempt":
					rows[0]["run_attempt"] = 3
				case "foreign URL":
					rows[0]["check_run_url"] = "https://other.invalid/repos/owner/repo/check-runs/1001"
				case "null jobs":
					rows = nil
					total = 0
				}
				_ = json.NewEncoder(w).Encode(actionsEnvelope("jobs", rows, total))
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").ObserveActionsRunAttempt(t.Context(), "owner/repo", 20, 50, 2)
			if err == nil || !reflect.DeepEqual(got, ActionsRunObservation{}) || calls.Load() > 9 {
				t.Fatal("partial inventory credited or bound exceeded", got, err, calls.Load())
			}
			if mode == "ninth page" && calls.Load() != 9 {
				t.Fatal("did not exercise eight page refusal", calls.Load())
			}
		})
	}
}
func originLink(origin string, page int) string {
	return fmt.Sprintf("<%s%s?page=%d&per_page=100>; rel=next", origin, actionsJobsPath, page)
}

func TestObserveActionsRunAttemptCancellationAndSelectorRefusal(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == actionsCurrentPath {
			_ = json.NewEncoder(w).Encode(actionsRunFixture())
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	g := testGitHub(server, "")
	for _, tc := range []struct {
		repo                 string
		repoID, run, attempt int64
	}{{"bad/path/extra", 20, 50, 2}, {"owner/repo", 0, 50, 2}, {"owner/repo", 20, 0, 2}, {"owner/repo", 20, 50, 0}} {
		got, err := g.ObserveActionsRunAttempt(t.Context(), tc.repo, tc.repoID, tc.run, tc.attempt)
		if err == nil || !reflect.DeepEqual(got, ActionsRunObservation{}) {
			t.Fatal(got, err)
		}
	}
	if _, err := g.ObserveActionsRunAttempt(nil, "owner/repo", 20, 50, 2); err == nil {
		t.Fatal("nil context accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selectors made requests")
	}
	var absent *GitHub
	if _, err := absent.ObserveActionsRunAttempt(t.Context(), "owner/repo", 20, 50, 2); err == nil {
		t.Fatal("nil adapter accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := g.ObserveActionsRunAttempt(ctx, "owner/repo", 20, 50, 2)
	if err == nil || !reflect.DeepEqual(got, ActionsRunObservation{}) || time.Since(start) > time.Second || calls.Load() != 2 {
		t.Fatal("deadline did not bound exact read", got, err, calls.Load())
	}
	ctx2, stop := context.WithCancel(t.Context())
	stop()
	got, err = g.ObserveActionsRunAttempt(ctx2, "owner/repo", 20, 50, 2)
	if err == nil || !reflect.DeepEqual(got, ActionsRunObservation{}) || calls.Load() != 2 {
		t.Fatal("cancelled call reached provider", got, err, calls.Load())
	}
}

func TestObserveActionsRunAttemptSharedDeadlineAndEmptyQueuedInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == actionsCurrentPath {
			run := actionsRunFixture()
			run["status"] = "queued"
			run["conclusion"] = nil
			_ = json.NewEncoder(w).Encode(run)
			return
		}
		if r.URL.Path != actionsJobsPath {
			t.Error("unexpected resource")
		}
		_ = json.NewEncoder(w).Encode(actionsEnvelope("jobs", []map[string]any{}, 0))
	}))
	defer server.Close()
	g := testGitHub(server, "")
	g.httpClient.Timeout = 0 // Inspect shared aggregate context, independent of per-request transport caps.
	underlying := g.httpClient.Transport
	var deadlines []time.Time
	g.httpClient.Transport = actionsRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("missing deadline")
		}
		deadlines = append(deadlines, deadline)
		return underlying.RoundTrip(r)
	})
	start := time.Now()
	got, err := g.ObserveActionsRunAttempt(t.Context(), "owner/repo", 20, 50, 2)
	if err != nil || got.Run.Status != "queued" || got.Run.Conclusion != "" || got.Jobs == nil || len(got.Jobs) != 0 || len(deadlines) != 3 {
		t.Fatal(got, err, deadlines)
	}
	for _, deadline := range deadlines {
		if !deadline.Equal(deadlines[0]) || deadline.Before(start.Add(14*time.Second)) || deadline.After(time.Now().Add(15*time.Second)) {
			t.Fatal("aggregate deadline reset", deadlines)
		}
	}
}
