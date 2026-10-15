package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const actionsHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const actionsRunsPath = "/repos/owner/repo/actions/runs"
const actionsCurrentPath = actionsRunsPath + "/50"
const actionsJobsPath = actionsCurrentPath + "/attempts/2/jobs"

func actionsRunFixture() map[string]any {
	return map[string]any{"id": 50, "head_sha": actionsHead, "check_suite_id": 40, "workflow_id": 30, "run_number": 7, "run_attempt": 2, "status": "completed", "conclusion": "success", "event": "pull_request", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z", "repository": map[string]any{"id": 20, "full_name": "owner/repo"}, "head_repository": map[string]any{"id": 999, "full_name": "fork/repo"}}
}

func actionsJobFixture(id int64) map[string]any {
	return map[string]any{"id": id, "run_id": 50, "head_sha": actionsHead, "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/" + strconv.FormatInt(id+1000, 10), "name": "test [linux]", "status": "completed", "conclusion": "success", "started_at": "2026-01-01T00:00:01.000Z", "completed_at": "2026-01-01T00:00:02+00:00", "logs_url": "PRIVATE_LOG_URL", "runner_name": "PRIVATE_RUNNER"}
}

func actionsEnvelope(key string, rows []map[string]any, total int) map[string]any {
	return map[string]any{"total_count": total, key: rows}
}

func actionsDefaultResponse(r *http.Request) any {
	switch r.URL.Path {
	case actionsRunsPath:
		return actionsEnvelope("workflow_runs", []map[string]any{actionsRunFixture()}, 1)
	case actionsCurrentPath:
		return actionsRunFixture()
	case actionsJobsPath:
		return actionsEnvelope("jobs", []map[string]any{actionsJobFixture(70)}, 1)
	default:
		return nil
	}
}

func TestCurrentActionsAttemptExactCurrentOnlyJobsAndSafeFacts(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	var origin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-secret" {
			t.Error("attempt reader changed method or credential")
		}
		switch r.URL.Path {
		case actionsRunsPath:
			q := r.URL.Query()
			if len(q) != 4 || q.Get("check_suite_id") != "40" || q.Get("head_sha") != actionsHead || q.Get("page") != "1" || q.Get("per_page") != "100" {
				t.Error("run selectors not exact", r.URL.String())
			}
			_ = json.NewEncoder(w).Encode(actionsEnvelope("workflow_runs", []map[string]any{actionsRunFixture()}, 1))
		case actionsCurrentPath:
			if r.URL.RawQuery != "" {
				t.Error("current-run request added selectors")
			}
			_ = json.NewEncoder(w).Encode(actionsRunFixture())
		case actionsJobsPath:
			page := r.URL.Query().Get("page")
			if len(r.URL.Query()) != 2 || r.URL.Query().Get("per_page") != "100" {
				t.Error("jobs selectors changed")
			}
			job := actionsJobFixture(70)
			if page == "1" {
				w.Header().Set("Link", "<"+origin+actionsJobsPath+"?per_page=100&page=2>; rel=next")
			} else if page == "2" {
				job = actionsJobFixture(71)
				job["run_attempt"] = 2
			} else {
				t.Error("unexpected jobpage", page)
			}
			_ = json.NewEncoder(w).Encode(actionsEnvelope("jobs", []map[string]any{job}, 2))
		default:
			t.Error("read old attempt/unrelated resource", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	origin = server.URL
	before := time.Now().UTC()
	got, err := testGitHub(server, "synthetic-secret").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
	if err != nil || got.State != "complete" || got.Reason != "" || got.RepositoryID != 20 || got.Repository != "owner/repo" || got.RunID != 50 || got.WorkflowID != 30 || got.RunAttempt != 2 || got.RunNumber != 7 || got.Event != "pull_request" || got.CheckSuiteID != 40 || got.HeadSHA != actionsHead || len(got.Jobs) != 2 || got.Jobs[0].CheckRunID != 1070 || got.Jobs[1].CheckRunID != 1071 || got.Jobs[0].CompletedAt.Location() != time.UTC || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now().UTC()) {
		t.Fatal("current attempt identity/facts lost", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 5 || !strings.HasPrefix(paths[0], actionsRunsPath+"?") || strings.TrimSuffix(paths[1], "?") != actionsCurrentPath || !strings.HasPrefix(paths[2], actionsJobsPath+"?") || strings.TrimSuffix(paths[4], "?") != actionsCurrentPath {
		t.Fatal("current attempt observation order changed", paths)
	}
	body, _ := json.Marshal(got)
	for _, secret := range []string{"PRIVATE_LOG_URL", "PRIVATE_RUNNER", "synthetic-secret", "head_repository", "merge_ready"} {
		if strings.Contains(string(body), secret) {
			t.Fatal("safe DTO expanded privatefacts", string(body))
		}
	}
}

func TestCurrentActionsAttemptPendingAndFailureAreInventoriesNotPasses(t *testing.T) {
	for _, state := range []string{"queued", "in_progress", "waiting", "pending", "requested", "failure", "skipped"} {
		t.Run(state, func(t *testing.T) {
			run, job := actionsRunFixture(), actionsJobFixture(70)
			if state == "failure" || state == "skipped" {
				run["conclusion"], job["conclusion"] = state, state
				if state == "skipped" {
					job["started_at"] = nil
				}
			} else {
				run["status"], run["conclusion"], job["status"], job["conclusion"], job["completed_at"] = state, nil, state, nil, nil
				if state != "in_progress" {
					job["started_at"] = nil
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any = run
				if r.URL.Path == actionsRunsPath {
					body = actionsEnvelope("workflow_runs", []map[string]any{run}, 1)
				}
				if r.URL.Path == actionsJobsPath {
					body = actionsEnvelope("jobs", []map[string]any{job}, 1)
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
			if err != nil || got.State != "complete" || len(got.Jobs) != 1 || got.Jobs[0].Status != job["status"] || state == "failure" && got.Jobs[0].Conclusion != "failure" {
				t.Fatal("inventory flattened pending/failure into permission", got, err)
			}
		})
	}
}

func TestCurrentActionsAttemptMissingAmbiguousAndObservationMovement(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous", "attempt", "head", "suite", "workflow", "repository_id", "repository_name", "run_number", "event", "status", "conclusion"} {
		t.Run(mode, func(t *testing.T) {
			var reads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := actionsDefaultResponse(r)
				if r.URL.Path == actionsRunsPath {
					rows := []map[string]any{actionsRunFixture()}
					if mode == "missing" {
						rows = []map[string]any{}
					}
					if mode == "ambiguous" {
						other := actionsRunFixture()
						other["id"] = 51
						rows = append(rows, other)
					}
					body = actionsEnvelope("workflow_runs", rows, len(rows))
				}
				if r.URL.Path == actionsCurrentPath && reads.Add(1) == 2 {
					run := actionsRunFixture()
					switch mode {
					case "attempt":
						run["run_attempt"] = 3
					case "head":
						run["head_sha"] = strings.Repeat("b", 40)
					case "suite":
						run["check_suite_id"] = 41
					case "workflow":
						run["workflow_id"] = 31
					case "repository_id":
						run["repository"].(map[string]any)["id"] = 21
					case "repository_name":
						run["repository"].(map[string]any)["full_name"] = "foreign/repo"
					case "run_number":
						run["run_number"] = 8
					case "event":
						run["event"] = "workflow_dispatch"
					case "status":
						run["status"], run["conclusion"] = "in_progress", nil
					case "conclusion":
						run["conclusion"] = "failure"
					}
					body = run
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
			want := "ci_attempt_changed"
			if mode == "missing" {
				want = "ci_attempt_missing"
			}
			if mode == "ambiguous" {
				want = "ci_attempt_ambiguous"
			}
			if mode == "status" || mode == "conclusion" {
				want = "ci_observation_changed"
			}
			if err != nil || got.State != "unproven" || got.Reason != want || len(got.Jobs) != 0 || got.Event != "" || got.RunNumber != 0 {
				t.Fatal("changed/absent currentattempt granted inventory", got, err)
			}
			if (mode == "missing" || mode == "ambiguous") && reads.Load() != 0 {
				t.Fatal("ambiguous runs caused unrelated selection")
			}
		})
	}
}

func TestCurrentActionsAttemptMalformedIdentityAndJobURLsFailClosed(t *testing.T) {
	cases := map[string]struct {
		kind string
		edit func(map[string]any)
	}{
		"foreign suite":             {"run", func(v map[string]any) { v["check_suite_id"] = 41 }},
		"foreign head":              {"run", func(v map[string]any) { v["head_sha"] = strings.Repeat("b", 40) }},
		"foreign repo":              {"run", func(v map[string]any) { v["repository"].(map[string]any)["full_name"] = "foreign/repo" }},
		"repo id0":                  {"run", func(v map[string]any) { v["repository"].(map[string]any)["id"] = 0 }},
		"attempt0":                  {"run", func(v map[string]any) { v["run_attempt"] = 0 }},
		"unknown run state":         {"run", func(v map[string]any) { v["status"] = "future" }},
		"unknown conclusion":        {"run", func(v map[string]any) { v["conclusion"] = "future" }},
		"null completed conclusion": {"run", func(v map[string]any) { v["conclusion"] = nil }},
		"missing required runfield": {"run", func(v map[string]any) { delete(v, "workflow_id") }},
		"run attempt numbertype":    {"run", func(v map[string]any) { v["run_attempt"] = 1.5 }},
		"job wrongrun":              {"job", func(v map[string]any) { v["run_id"] = 51 }},
		"job oldattempt":            {"job", func(v map[string]any) { v["run_attempt"] = 1 }},
		"job nullattempt":           {"job", func(v map[string]any) { v["run_attempt"] = nil }},
		"job wronghead":             {"job", func(v map[string]any) { v["head_sha"] = strings.Repeat("b", 40) }},
		"job alias":                 {"job", func(v map[string]any) { v["Run_id"] = v["run_id"]; delete(v, "run_id") }},
		"job badtime":               {"job", func(v map[string]any) { v["started_at"] = "not-a-time" }},
		"job reversedtime":          {"job", func(v map[string]any) { v["completed_at"] = "2025-01-01T00:00:00Z" }},
		"job nullcompletion":        {"job", func(v map[string]any) { v["completed_at"] = nil }},
		"job unknownstatus":         {"job", func(v map[string]any) { v["status"] = "future" }},
		"job controls":              {"job", func(v map[string]any) { v["name"] = "private\x00" }},
	}
	for _, raw := range []string{"http://api.github.com/repos/owner/repo/check-runs/1070", "https://api.github.com@evil.invalid/repos/owner/repo/check-runs/1070", "https://api.github.com/repos/foreign/repo/check-runs/1070", "https://api.github.com/repos/owner/repo/check-runs/0", "https://api.github.com/repos/owner/repo/check-runs/01070", "https://api.github.com/repos/owner/repo/check-runs/1070?x=1", "https://api.github.com/repos/owner/repo/check-runs/%31", "https://api.github.com/repos/owner/repo/check-runs/9223372036854775808", "https://api.github.com/repos/owner/repo/check-runs/1070#x"} {
		cases[raw] = struct {
			kind string
			edit func(map[string]any)
		}{"job", func(v map[string]any) { v["check_run_url"] = raw }}
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body := actionsDefaultResponse(r)
				if test.kind == "run" && r.URL.Path == actionsRunsPath {
					run := actionsRunFixture()
					test.edit(run)
					body = actionsEnvelope("workflow_runs", []map[string]any{run}, 1)
				}
				if test.kind == "job" && r.URL.Path == actionsJobsPath {
					job := actionsJobFixture(70)
					test.edit(job)
					body = actionsEnvelope("jobs", []map[string]any{job}, 1)
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "synthetic-secret").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
			if !hasCode(err, "provider_invalid_response") || got.State != "unproven" || len(got.Jobs) != 0 || calls.Load() > 3 {
				t.Fatal("malformed capture granted complete proof or continued reads", got, err, calls.Load())
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "evil.invalid") {
				t.Fatal("private provider input leaked")
			}
		})
	}
}

func TestCurrentActionsAttemptPaginationCompletenessAndFixedSelectors(t *testing.T) {
	for _, kind := range []string{"runs", "jobs"} {
		for _, mode := range []string{"missingpage", "jump", "changedselector", "countdrift", "duplicate", "duplicatecheck", "null", "overflow", "pagefailure", "redirect", "duplicatekey"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				var origin string
				var requested atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						t.Error("pagination mutated provider")
					}
					path := actionsRunsPath
					key := "workflow_runs"
					row := actionsRunFixture()
					if kind == "jobs" {
						path = actionsJobsPath
						key = "jobs"
						row = actionsJobFixture(70)
					}
					if r.URL.Path != path {
						_ = json.NewEncoder(w).Encode(actionsDefaultResponse(r))
						return
					}
					page := r.URL.Query().Get("page")
					requested.Add(1)
					if page == "3" {
						t.Error("skippedpage was requested")
					}
					if page == "2" && mode == "pagefailure" {
						w.WriteHeader(503)
						return
					}
					if mode == "redirect" {
						http.Redirect(w, r, origin+"/unrelated", 302)
						return
					}
					if r.URL.Path == "/unrelated" {
						t.Error("redirect was followed")
					}
					total := 2
					if mode == "overflow" {
						total = githubMaxItems + 1
					}
					if mode == "null" {
						_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, key: nil})
						return
					}
					if mode == "duplicatekey" {
						_, _ = fmt.Fprintf(w, `{"total_count":0,"total_count":0,%q:[]}`, key)
						return
					}
					if page == "1" && mode != "missingpage" {
						query := url.Values{"page": {"2"}, "per_page": {"100"}}
						if kind == "runs" {
							query.Set("check_suite_id", "40")
							query.Set("head_sha", actionsHead)
						}
						if mode == "jump" {
							query.Set("page", "3")
						}
						if mode == "changedselector" {
							query.Set("head_sha", strings.Repeat("b", 40))
						}
						w.Header().Set("Link", "<"+origin+path+"?"+query.Encode()+">; rel=next")
					}
					if page == "2" {
						if mode == "countdrift" {
							total = 3
						}
						if mode != "duplicate" {
							if kind == "runs" {
								row["id"] = 51
							} else {
								row["id"] = 71
								if mode != "duplicatecheck" {
									row["check_run_url"] = "https://api.github.com/repos/owner/repo/check-runs/1071"
								}
							}
						}
					}
					_ = json.NewEncoder(w).Encode(actionsEnvelope(key, []map[string]any{row}, total))
				}))
				defer server.Close()
				origin = server.URL
				got, err := testGitHub(server, "").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
				if kind == "runs" && mode == "duplicatecheck" {
					if err != nil || got.State != "unproven" || got.Reason != "ci_attempt_ambiguous" || requested.Load() != 2 {
						t.Fatal("complete paginated multiple-run inventory was not held", got, err)
					}
					return
				}
				if err == nil || got.State != "unproven" || len(got.Jobs) != 0 {
					t.Fatal("partial/changedinventory accepted", got, err)
				}
				if (mode == "jump" || mode == "changedselector") && requested.Load() != 1 {
					t.Fatal("untrusted link followed", requested.Load())
				}
			})
		}
	}
}

type actionsRoundTrip func(*http.Request) (*http.Response, error)

func (f actionsRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCurrentActionsAttemptSingleAggregateDeadlineAndCancellation(t *testing.T) {
	var calls atomic.Int64
	closed := make(chan struct{}, 1)
	var block atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == actionsJobsPath && block.Load() {
			<-r.Context().Done()
			closed <- struct{}{}
			return
		}
		_ = json.NewEncoder(w).Encode(actionsDefaultResponse(r))
	}))
	defer server.Close()
	g := testGitHub(server, "")
	g.httpClient.Timeout = 0 // Inspect the method's aggregate context, independent of the separately owned per-request cap.
	underlying := g.httpClient.Transport
	var deadlines []time.Time
	var mu sync.Mutex
	g.httpClient.Transport = actionsRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("aggregate deadline missing")
		}
		mu.Lock()
		deadlines = append(deadlines, deadline)
		mu.Unlock()
		return underlying.RoundTrip(r)
	})
	before := time.Now()
	if got, err := g.CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40); err != nil || got.State != "complete" {
		t.Fatal(got, err)
	}
	if len(deadlines) != 4 {
		t.Fatal("unexpected requests", len(deadlines))
	}
	for _, deadline := range deadlines {
		if !deadline.Equal(deadlines[0]) || deadline.Before(before.Add(14*time.Second)) || deadline.After(time.Now().Add(15*time.Second)) {
			t.Fatal("eachrequestresetaggregatebudget", deadlines)
		}
	}
	block.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	got, err := g.CurrentActionsAttempt(ctx, "owner/repo", actionsHead, 40)
	if !hasCode(err, "provider_unavailable") || got.State != "unproven" || calls.Load() != 7 {
		t.Fatal("cancellation was retried/continued", got, err, calls.Load())
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation left jobs HTTP open")
	}
	interrupted, stop := context.WithCancel(t.Context())
	stop()
	_, err = g.CurrentActionsAttempt(interrupted, "owner/repo", actionsHead, 40)
	if err == nil || calls.Load() != 7 {
		t.Fatal("pre-canceledreadperformedHTTP", err)
	}
}

func TestCurrentActionsAttemptInvalidSelectorsAndCaseInsensitiveRepository(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		selected := r.Clone(r.Context())
		address := *r.URL
		address.Path = strings.ToLower(address.Path)
		selected.URL = &address
		_ = json.NewEncoder(w).Encode(actionsDefaultResponse(selected))
	}))
	defer server.Close()
	g := testGitHub(server, "")
	for _, input := range []struct {
		ctx        context.Context
		repo, head string
		suite      int64
	}{{nil, "owner/repo", actionsHead, 40}, {t.Context(), "../repo", actionsHead, 40}, {t.Context(), "owner/repo", strings.Repeat("A", 40), 40}, {t.Context(), "owner/repo", actionsHead, 0}} {
		if _, err := g.CurrentActionsAttempt(input.ctx, input.repo, input.head, input.suite); err == nil {
			t.Fatal("invalidselectorsaccepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalidselectorscontactedprovider")
	}
	var missing *GitHub
	if _, err := missing.CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40); err == nil {
		t.Fatal("niladapteraccepted")
	}
	// The request selector is case-insensitive; URLs returned by the provider
	// still bind its exact canonical repository spelling.
	got, err := g.CurrentActionsAttempt(t.Context(), "OWNER/REPO", actionsHead, 40)
	if err != nil || got.State != "complete" || got.RepositoryID != 20 || got.Repository != "owner/repo" || calls.Load() != 4 {
		t.Fatal("case-insensitive documented repository selector rejected", got, err)
	}
}

func TestCurrentActionsAttemptAggregateBytesAndPaginationBound(t *testing.T) {
	for _, mode := range []string{"bytes", "pages"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			var origin string
			padding := ""
			if mode == "bytes" {
				padding = strings.Repeat("x", 6<<20)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body := actionsDefaultResponse(r).(map[string]any)
				if mode == "bytes" {
					body["future_metadata"] = padding
				} else if r.URL.Path == actionsJobsPath {
					page, _ := strconv.ParseInt(r.URL.Query().Get("page"), 10, 64)
					job := actionsJobFixture(70 + page)
					body = actionsEnvelope("jobs", []map[string]any{job}, githubMaxItems)
					w.Header().Set("Link", "<"+origin+actionsJobsPath+"?per_page=100&page="+strconv.FormatInt(page+1, 10)+">; rel=next")
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
			if !hasCode(err, "provider_invalid_response") || got.State != "unproven" || len(got.Jobs) != 0 {
				t.Fatal("aggregate capacity became truncated proof", got, err)
			}
			if mode == "bytes" && calls.Load() != 3 || mode == "pages" && calls.Load() != githubMaxPages+1 {
				t.Fatal("shared capacity was not bounded before later reads", calls.Load())
			}
		})
	}
}

func TestCurrentActionsAttemptRunNumberRequiredAcrossObservation(t *testing.T) {
	for _, stage := range []string{"list", "current", "final"} {
		for _, mode := range []string{"missing", "null", "zero", "negative", "fractional", "alias", "changed"} {
			if stage != "current" && mode == "changed" {
				continue
			}
			t.Run(stage+"/"+mode, func(t *testing.T) {
				var reads atomic.Int64
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body := actionsDefaultResponse(r)
					edit := stage == "list" && r.URL.Path == actionsRunsPath
					if r.URL.Path == actionsCurrentPath {
						n := reads.Add(1)
						edit = stage == "current" && n == 1 || stage == "final" && n == 2
					}
					if edit {
						run := actionsRunFixture()
						switch mode {
						case "missing":
							delete(run, "run_number")
						case "null":
							run["run_number"] = nil
						case "zero":
							run["run_number"] = 0
						case "negative":
							run["run_number"] = -1
						case "fractional":
							run["run_number"] = 1.5
						case "alias":
							delete(run, "run_number")
							run["Run_number"] = 7
						case "changed":
							run["run_number"] = 8
						}
						body = run
						if stage == "list" {
							body = actionsEnvelope("workflow_runs", []map[string]any{run}, 1)
						}
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				got, err := testGitHub(server, "").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
				wantCalls := int64(1)
				if stage == "current" {
					wantCalls = 2
				}
				if stage == "final" {
					wantCalls = 4
				}
				if !hasCode(err, "provider_invalid_response") || got.State != "unproven" || len(got.Jobs) != 0 || got.Event != "" || got.RunNumber != 0 || calls.Load() != wantCalls {
					t.Fatal("invalid or inconsistent run number granted inventory or continued reads", got, err, calls.Load())
				}
			})
		}
	}
}

func TestCurrentActionsAttemptRetainsActualEventAndRunNumberWithoutApplicabilityVerdict(t *testing.T) {
	for _, event := range []string{"pull_request", "push", "workflow_dispatch", "pull_request_target"} {
		t.Run(event, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := actionsDefaultResponse(r)
				if r.URL.Path == actionsRunsPath || r.URL.Path == actionsCurrentPath {
					run := actionsRunFixture()
					run["event"], run["run_number"] = event, int64(9223372036854775807)
					body = run
					if r.URL.Path == actionsRunsPath {
						body = actionsEnvelope("workflow_runs", []map[string]any{run}, 1)
					}
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
			if err != nil || got.State != "complete" || got.Event != event || got.RunNumber != 9223372036854775807 {
				t.Fatal("actual workflow identity lost", got, err)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["event"]) != strconv.Quote(event) || string(fields["run_number"]) != "9223372036854775807" {
				t.Fatal("public JSON lost exact event/run number", string(raw))
			}
		})
	}
}
