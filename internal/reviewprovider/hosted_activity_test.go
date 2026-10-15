package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const activityHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func activityStatus(id int64, state string) map[string]any {
	return map[string]any{"id": id, "context": "CodeRabbit", "state": state, "creator": map[string]any{"id": 30, "login": "synthetic-provider[bot]", "type": "Bot"}, "created_at": "2026-01-01T00:00:01Z", "updated_at": "2026-01-01T00:00:02Z"}
}

func activityCheck(id int64, state string) map[string]any {
	r := map[string]any{"id": id, "name": "CodeRabbit", "head_sha": activityHead, "status": state, "app": map[string]any{"id": 42}, "started_at": nil, "completed_at": nil}
	if state == "completed" {
		r["started_at"], r["completed_at"] = "2026-01-01T00:00:01Z", "2026-01-01T00:00:02Z"
	}
	return r
}

func activitySelector(mode string) HostedActivitySelector {
	s := HostedActivitySelector{Mode: mode, ProviderActorID: 30}
	if mode == "check_run" {
		s.AppID = 42
	}
	return s
}

func activityPayload(mode string, rows []map[string]any, total int) any {
	if mode == "commit_status" {
		return rows
	}
	return map[string]any{"total_count": total, "check_runs": rows}
}

func TestHostedActivityStatusLatestStableActorAndNotReviewCoverage(t *testing.T) {
	for _, state := range []string{"pending", "success", "failure", "error", "future_state"} {
		t.Run(state, func(t *testing.T) {
			latest := activityStatus(5, state)
			latest["description"] = "Review skipped: automatic reviews are disabled"
			latest["context"] = "cOdErAbBiT"
			latest["creator"].(map[string]any)["login"] = "renamed-provider[bot]"
			old := activityStatus(900, "pending")
			old["created_at"], old["updated_at"] = "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"
			spoof := activityStatus(9999, "success")
			spoof["creator"].(map[string]any)["id"] = 31
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode([]any{old, spoof, latest}) }))
			defer server.Close()
			before := time.Now().UTC()
			got, err := testGitHub(server, "synthetic-secret").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector("commit_status"))
			want := "idle"
			if state == "pending" {
				want = "active"
			}
			if state == "future_state" {
				want = "unproven"
			}
			if err != nil || got.State != want || got.SourceID != 5 || got.ObservedAt.Location() != time.UTC || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now().UTC()) || got.CreatedAt.Format(time.RFC3339Nano) != "2026-01-01T00:00:01Z" {
				t.Fatalf("activity/status identity: %+v %v", got, err)
			}
			body, _ := json.Marshal(got)
			for _, unsupported := range []string{"Review skipped", "coverage", "review_id", "synthetic-secret"} {
				if strings.Contains(string(body), unsupported) {
					t.Fatal("activity inferred coverage or exposed prose", string(body))
				}
			}
		})
	}
}

func TestHostedActivityCommitStatusCanonicalTieAndAmbiguity(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		first, second := activityStatus(1, "pending"), activityStatus(2, "success")
		if duplicate {
			second["id"] = 1
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode([]any{second, first}) }))
		defer server.Close()
		got, err := testGitHub(server, "").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector("commit_status"))
		if err != nil || duplicate && (got.State != "unproven" || got.Reason != "activity_ambiguous") || !duplicate && (got.State != "idle" || got.SourceID != 2) {
			t.Fatal("same-second identity/duplicates", got, err)
		}
	}
}

func TestHostedActivityCheckActiveCannotBeMaskedAndNullStartIsAllowed(t *testing.T) {
	for _, state := range []string{"queued", "in_progress", "waiting", "requested", "pending", "completed", "unknown"} {
		t.Run(state, func(t *testing.T) {
			selected := activityCheck(1, state)
			completed := activityCheck(999, "completed")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(activityPayload("check_run", []map[string]any{completed, selected}, 2))
			}))
			defer server.Close()
			got, err := testGitHub(server, "").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector("check_run"))
			want := "active"
			if state == "completed" {
				want = "idle"
			}
			if state == "unknown" {
				want = "unproven"
			}
			if err != nil || got.State != want || want == "active" && (got.SourceID != 1 || !got.StartedAt.IsZero()) {
				t.Fatal("completed check masked possibly active run", got, err)
			}
		})
	}
}

func TestHostedActivityUntrustedAndMissingSignalsReportScopedAbsence(t *testing.T) {
	for _, mode := range []string{"commit_status", "check_run"} {
		for _, spoof := range []string{"empty", "name", "actor or app", "kind or head"} {
			t.Run(mode+"/"+spoof, func(t *testing.T) {
				row := activityStatus(1, "success")
				if mode == "check_run" {
					row = activityCheck(1, "completed")
				}
				switch spoof {
				case "name":
					if mode == "check_run" {
						row["name"] = "CodeRabbit companion"
					} else {
						row["context"] = "CI / CodeRabbit companion"
					}
				case "actor or app":
					if mode == "check_run" {
						row["app"].(map[string]any)["id"] = 43
					} else {
						row["creator"].(map[string]any)["id"] = 31
					}
				case "kind or head":
					if mode == "check_run" {
						row["head_sha"] = strings.Repeat("b", 40)
					} else {
						row["creator"].(map[string]any)["type"] = "User"
					}
				}
				rows := []map[string]any{row}
				if spoof == "empty" {
					rows = []map[string]any{}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(activityPayload(mode, rows, len(rows)))
				}))
				defer server.Close()
				got, err := testGitHub(server, "").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector(mode))
				if err != nil || got.State != "idle" || got.Reason != "no_provider_activity_observed" || got.SourceID != 0 || got.ObservedAt.IsZero() {
					t.Fatal("complete scoped absence fabricated a trusted source", got, err)
				}
			})
		}
	}
}

func TestHostedActivityTwoPagesPreserveFixedQueryAndOriginalPath(t *testing.T) {
	for _, mode := range []string{"commit_status", "check_run"} {
		t.Run(mode, func(t *testing.T) {
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.RequestURI())
				collection := "statuses"
				params := 2
				if mode == "check_run" {
					collection = "check-runs"
					params = 3
				}
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-secret" || r.URL.Path != "/repos/owner/repo/commits/"+activityHead+"/"+collection || len(r.URL.Query()) != params || r.URL.Query().Get("per_page") != "100" || mode == "check_run" && r.URL.Query().Get("filter") != "all" {
					t.Error("unexpected route/headers/query", r.URL)
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				if page == 1 {
					extra := ""
					if mode == "check_run" {
						extra = "&filter=all"
					}
					w.Header().Set("Link", fmt.Sprintf("<%s/repositories/901/commits/%s/%s?page=2&per_page=100%s>; rel=next", serverURL(r), activityHead, collection, extra))
				}
				row := activityStatus(int64(page), "pending")
				if mode == "check_run" {
					row = activityCheck(int64(page), "queued")
				}
				_ = json.NewEncoder(w).Encode(activityPayload(mode, []map[string]any{row}, 2))
			}))
			defer server.Close()
			got, err := testGitHub(server, "synthetic-secret").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector(mode))
			if err != nil || got.State != "active" || len(calls) != 2 {
				t.Fatal("complete two-page observation", got, err, calls)
			}
		})
	}
}

func TestHostedActivityPaginationAndPartialFailuresNeverIdle(t *testing.T) {
	for _, mode := range []string{"commit_status", "check_run"} {
		for _, bad := range []string{"skip", "origin", "fixed query", "page error", "redirect", "missing page", "null", "duplicate JSON", "field alias", "too many rows", "body limit"} {
			if mode == "commit_status" && bad == "missing page" {
				continue // This endpoint has no advertised total; a linked-page failure is tested separately.
			}
			t.Run(mode+"/"+bad, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if bad == "redirect" {
						w.Header().Set("Location", "/must-not-follow")
						w.WriteHeader(http.StatusFound)
						return
					}
					if bad == "page error" && r.URL.Query().Get("page") == "2" {
						w.WriteHeader(503)
						_, _ = w.Write([]byte("private provider diagnostic"))
						return
					}
					if bad == "null" {
						_, _ = w.Write([]byte("null"))
						return
					}
					if bad == "duplicate JSON" {
						if mode == "check_run" {
							_, _ = w.Write([]byte(`{"total_count":0,"total_count":0,"check_runs":[]}`))
						} else {
							_, _ = w.Write([]byte(`[{"id":1,"id":2}]`))
						}
						return
					}
					if bad == "field alias" {
						if mode == "check_run" {
							_, _ = w.Write([]byte(`{"total_count":0,"Check_Runs":[]}`))
						} else {
							_, _ = w.Write([]byte(`[{"ID":1}]`))
						}
						return
					}
					if bad == "body limit" {
						_, _ = w.Write([]byte(strings.Repeat(" ", githubMaxBody+1)))
						return
					}
					if bad == "skip" || bad == "origin" || bad == "fixed query" || bad == "page error" {
						page := "2"
						origin := serverURL(r)
						extra := ""
						if mode == "check_run" {
							extra = "&filter=all"
						}
						if bad == "skip" {
							page = "3"
						}
						if bad == "origin" {
							origin = "https://foreign.invalid"
						}
						if bad == "fixed query" {
							extra = "&filter=latest"
						}
						w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%s&per_page=100%s>; rel=next", origin, r.URL.Path, page, extra))
					}
					row := activityStatus(1, "success")
					if mode == "check_run" {
						row = activityCheck(1, "completed")
					}
					rows := []map[string]any{row}
					total := 2
					if bad == "too many rows" {
						rows = make([]map[string]any, 101)
						for i := range rows {
							rows[i] = row
						}
						total = 101
					}
					_ = json.NewEncoder(w).Encode(activityPayload(mode, rows, total))
				}))
				defer server.Close()
				got, err := testGitHub(server, "synthetic-secret").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector(mode))
				if err == nil || got.State != "unproven" || !got.ObservedAt.IsZero() || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "synthetic-secret") {
					t.Fatal("unsafe partial observation", got, err)
				}
				want := int64(1)
				if bad == "page error" {
					want = 2
				}
				if calls.Load() != want {
					t.Fatal("unexpected follow/retry", calls.Load(), want)
				}
			})
		}
	}
}

func TestHostedActivityCheckDuplicatesAndTimeValidation(t *testing.T) {
	for _, bad := range []string{"exact duplicate", "partial duplicate", "conflicting duplicate", "missing finish", "reversed time", "offset time", "future time"} {
		t.Run(bad, func(t *testing.T) {
			first := activityCheck(1, "completed")
			second := activityCheck(1, "completed")
			if bad != "exact duplicate" && bad != "partial duplicate" && bad != "conflicting duplicate" {
				second["id"] = 2
			}
			switch bad {
			case "conflicting duplicate":
				second["status"] = "queued"
			case "missing finish":
				second["completed_at"] = nil
			case "reversed time":
				second["completed_at"] = "2025-12-31T23:59:59Z"
			case "offset time":
				second["started_at"] = "2026-01-01T00:00:01+00:00"
			case "future time":
				second["completed_at"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				total := 2
				if bad == "exact duplicate" || bad == "conflicting duplicate" {
					total = 1
				}
				_ = json.NewEncoder(w).Encode(activityPayload("check_run", []map[string]any{first, second}, total))
			}))
			defer server.Close()
			got, err := testGitHub(server, "").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector("check_run"))
			if bad == "exact duplicate" {
				if err != nil || got.State != "idle" {
					t.Fatal(got, err)
				}
			} else if got.State != "unproven" || err == nil && got.Reason != "activity_ambiguous" {
				t.Fatal("malformed/conflicting check became idle", got, err)
			}
		})
	}
}

func TestHostedActivitySelectorsCancellationAndSharedDeadline(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", fmt.Sprintf("<%s%s?page=2&per_page=100&filter=all>; rel=next", serverURL(r), r.URL.Path))
			_ = json.NewEncoder(w).Encode(activityPayload("check_run", []map[string]any{activityCheck(1, "queued")}, 2))
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	g := testGitHub(server, "")
	for _, selector := range []HostedActivitySelector{{}, {Mode: "commit_status", ProviderActorID: 30, AppID: 42}, {Mode: "check_run", ProviderActorID: 30}, {Mode: "check_run", AppID: 42}, {Mode: "CHECK_RUN", ProviderActorID: 30, AppID: 42}} {
		got, err := g.ObserveHostedActivity(t.Context(), "owner/repo", activityHead, selector)
		if err == nil || got.State != "unproven" || calls.Load() != 0 {
			t.Fatal("invalid selector reached HTTP", got, err)
		}
	}
	for _, head := range []string{"", strings.Repeat("A", 40), activityHead + "/other"} {
		if _, err := g.ObserveHostedActivity(t.Context(), "owner/repo", head, activitySelector("commit_status")); err == nil || calls.Load() != 0 {
			t.Fatal("invalid head reached HTTP")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.ObserveHostedActivity(ctx, "owner/repo", activityHead, activitySelector("check_run")); err == nil || calls.Load() != 0 {
		t.Fatal("cancelled call reached provider")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := g.ObserveHostedActivity(ctx, "owner/repo", activityHead, activitySelector("check_run"))
	if err == nil || got.State != "unproven" || !got.ObservedAt.IsZero() || calls.Load() != 2 || time.Since(started) > time.Second {
		t.Fatal("page failure ignored shared caller budget", got, err, calls.Load())
	}
}

func TestHostedActivityStatusRejectsMalformedAndNoncanonicalTimes(t *testing.T) {
	for _, bad := range []string{"missing", "offset", "future", "reversed", "zero ID"} {
		t.Run(bad, func(t *testing.T) {
			row := activityStatus(1, "success")
			switch bad {
			case "missing":
				delete(row, "created_at")
			case "offset":
				row["created_at"] = "2026-01-01T00:00:01+00:00"
			case "future":
				row["updated_at"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
			case "reversed":
				row["updated_at"] = "2026-01-01T00:00:00Z"
			case "zero ID":
				row["id"] = 0
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode([]any{row}) }))
			defer server.Close()
			got, err := testGitHub(server, "").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector("commit_status"))
			if err == nil || got.State != "unproven" || !got.ObservedAt.IsZero() {
				t.Fatal("malformed status acquired idle activity", got, err)
			}
		})
	}
}

func TestHostedActivityAggregateAndPageLimits(t *testing.T) {
	for _, mode := range []string{"commit_status", "check_run"} {
		for _, limit := range []string{"aggregate", "pages"} {
			t.Run(mode+"/"+limit, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					extra := ""
					if mode == "check_run" {
						extra = "&filter=all"
					}
					if limit == "pages" || page < 3 {
						w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100%s>; rel=next", serverURL(r), r.URL.Path, page+1, extra))
					}
					row := activityStatus(int64(page), "success")
					if mode == "check_run" {
						row = activityCheck(int64(page), "completed")
					}
					if limit == "aggregate" {
						row["unrelated_future_field"] = strings.Repeat("x", 6<<20)
					}
					total := 3
					if limit == "pages" {
						total = 100
					}
					_ = json.NewEncoder(w).Encode(activityPayload(mode, []map[string]any{row}, total))
				}))
				defer server.Close()
				got, err := testGitHub(server, "").ObserveHostedActivity(t.Context(), "owner/repo", activityHead, activitySelector(mode))
				want := int64(3)
				if limit == "pages" {
					want = githubMaxPages
				}
				if err == nil || got.State != "unproven" || !got.ObservedAt.IsZero() || calls.Load() != want {
					t.Fatal("unbounded collection was accepted or followed", got, err, calls.Load())
				}
			})
		}
	}
}
