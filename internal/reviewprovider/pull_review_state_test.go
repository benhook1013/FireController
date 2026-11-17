package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func pullReviewTarget() CITarget {
	return CITarget{RepositoryID: 19, PullNumber: 7, Repository: "owner/repo", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
}
func pullReviewPageFixture(total int, nodes []any, next bool, cursor any) map[string]any {
	t := pullReviewTarget()
	return map[string]any{"data": map[string]any{"repository": map[string]any{"fullDatabaseId": strconv.FormatInt(t.RepositoryID, 10), "nameWithOwner": "Owner/Repo", "pullRequest": map[string]any{
		"number": t.PullNumber, "headRefOid": t.HeadSHA, "baseRefOid": t.BaseSHA, "baseRefName": t.BaseRef, "state": "OPEN", "isDraft": false, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": nil,
		"reviewThreads": map[string]any{"totalCount": total, "nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}},
	}}}}
}
func pullReviewNode(id string, resolved, outdated bool) any {
	return map[string]any{"id": id, "isResolved": resolved, "isOutdated": outdated}
}
func pullReviewPull(body map[string]any) map[string]any {
	return body["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)
}
func pullReviewConnection(body map[string]any) map[string]any {
	return pullReviewPull(body)["reviewThreads"].(map[string]any)
}

func TestPullReviewStateCompleteSelectedTwoPassInventory(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/graphql" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer synthetic-secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("request escaped fixed authenticated GraphQL query")
		}
		var req struct {
			Query     string
			Variables struct {
				Owner, Name string
				Number      int64
				After       *string
			}
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Query != pullReviewStateQuery || strings.Contains(req.Query, "mutation") || req.Variables.Owner != "owner" || req.Variables.Name != "repo" || req.Variables.Number != 7 {
			t.Error("query/selector changed")
		}
		var b map[string]any
		if n%2 == 1 {
			if req.Variables.After != nil {
				t.Error("scan did not restart")
			}
			b = pullReviewPageFixture(3, []any{pullReviewNode("T3", false, true), pullReviewNode("T1", true, false)}, true, "cursor-1")
		} else {
			if req.Variables.After == nil || *req.Variables.After != "cursor-1" {
				t.Error("cursor not scoped")
			}
			b = pullReviewPageFixture(3, []any{pullReviewNode("T2", false, false)}, false, "cursor-2")
		}
		b["future"] = map[string]any{"ignored": true}
		_ = json.NewEncoder(w).Encode(b)
	}))
	defer s.Close()
	before := time.Now().UTC()
	got, err := testGitHub(s, "synthetic-secret").ObservePullReviewState(t.Context(), pullReviewTarget())
	want := []PullReviewThread{{"T1", true, false}, {"T2", false, false}, {"T3", false, true}}
	if err != nil || got.State != "complete" || got.Reason != "" || calls.Load() != 4 || !reflect.DeepEqual(got.Threads, want) || got.ReviewDecision != nil || got.PullState != "OPEN" || got.Mergeable != "MERGEABLE" || got.MergeStateStatus != "CLEAN" || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now()) || got.ObservedAt.Location() != time.UTC {
		t.Fatalf("observation %#v error %v calls %d", got, err, calls.Load())
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "synthetic-secret") || !strings.Contains(string(encoded), `"review_decision":null`) {
		t.Fatal("public DTO privacy or nullable shape changed")
	}
}

func TestPullReviewStateCoherentProviderFactsDoNotImplyReadiness(t *testing.T) {
	for _, row := range []struct {
		state, mergeable, mergeState string
		draft                        bool
		decision                     any
	}{{"OPEN", "UNKNOWN", "UNKNOWN", true, nil}, {"CLOSED", "CONFLICTING", "DIRTY", false, "CHANGES_REQUESTED"}, {"MERGED", "MERGEABLE", "CLEAN", false, "APPROVED"}, {"OPEN", "MERGEABLE", "BLOCKED", false, "REVIEW_REQUIRED"}} {
		t.Run(row.state+row.mergeState, func(t *testing.T) {
			var calls int
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				b := pullReviewPageFixture(0, []any{}, false, nil)
				p := pullReviewPull(b)
				p["state"], p["isDraft"], p["mergeable"], p["mergeStateStatus"], p["reviewDecision"] = row.state, row.draft, row.mergeable, row.mergeState, row.decision
				_ = json.NewEncoder(w).Encode(b)
			}))
			defer s.Close()
			got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget())
			if err != nil || got.State != "complete" || got.PullState != row.state || got.Draft != row.draft || got.Mergeable != row.mergeable || got.MergeStateStatus != row.mergeState || got.Threads == nil || len(got.Threads) != 0 || calls != 2 {
				t.Fatal(got, err, calls)
			}
		})
	}
}

func TestPullReviewStateMovementNeverReturnsPartialSuccess(t *testing.T) {
	for _, kind := range []string{"head", "base", "branch", "repository_id", "repository_name", "pull_number", "draft", "state", "mergeable", "merge_state", "decision", "thread_resolved", "thread_outdated", "thread_id", "thread_removed"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := pullReviewPageFixture(1, []any{pullReviewNode("T1", false, false)}, false, "c1")
				if calls.Add(1) == 2 {
					p := pullReviewPull(b)
					repo := b["data"].(map[string]any)["repository"].(map[string]any)
					switch kind {
					case "head":
						p["headRefOid"] = strings.Repeat("c", 40)
					case "base":
						p["baseRefOid"] = strings.Repeat("d", 40)
					case "branch":
						p["baseRefName"] = "other"
					case "repository_id":
						repo["fullDatabaseId"] = "20"
					case "repository_name":
						repo["nameWithOwner"] = "owner/other"
					case "pull_number":
						p["number"] = 8
					case "draft":
						p["isDraft"] = true
					case "state":
						p["state"] = "CLOSED"
					case "mergeable":
						p["mergeable"] = "UNKNOWN"
					case "merge_state":
						p["mergeStateStatus"] = "BLOCKED"
					case "decision":
						p["reviewDecision"] = "APPROVED"
					case "thread_resolved":
						pullReviewConnection(b)["nodes"] = []any{pullReviewNode("T1", true, false)}
					case "thread_outdated":
						pullReviewConnection(b)["nodes"] = []any{pullReviewNode("T1", false, true)}
					case "thread_id":
						pullReviewConnection(b)["nodes"] = []any{pullReviewNode("T2", false, false)}
					case "thread_removed":
						b = pullReviewPageFixture(0, []any{}, false, nil)
					}
				}
				_ = json.NewEncoder(w).Encode(b)
			}))
			defer s.Close()
			got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget())
			if err != nil || got.State != "unproven" || got.Reason != "review_state_changed" || got.Threads != nil || calls.Load() != 2 {
				t.Fatal(got, err, calls.Load())
			}
		})
	}
}

func TestPullReviewStateMalformedSelectedDataFailsClosed(t *testing.T) {
	cases := map[string]func(map[string]any){
		"null data":       func(b map[string]any) { b["data"] = nil },
		"null repository": func(b map[string]any) { b["data"].(map[string]any)["repository"] = nil },
		"null pull":       func(b map[string]any) { b["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"] = nil },
		"errors with data": func(b map[string]any) {
			b["errors"] = []any{map[string]any{"message": "private provider diagnostic synthetic-secret"}}
		},
		"missing nullable decision": func(b map[string]any) { delete(pullReviewPull(b), "reviewDecision") },
		"missing state":             func(b map[string]any) { delete(pullReviewPull(b), "state") },
		"null draft":                func(b map[string]any) { pullReviewPull(b)["isDraft"] = nil },
		"unknown state":             func(b map[string]any) { pullReviewPull(b)["state"] = "FUTURE" },
		"unknown merge":             func(b map[string]any) { pullReviewPull(b)["mergeable"] = "FUTURE" },
		"unknown gate":              func(b map[string]any) { pullReviewPull(b)["mergeStateStatus"] = "FUTURE" },
		"unknown decision":          func(b map[string]any) { pullReviewPull(b)["reviewDecision"] = "FUTURE" },
		"null connection":           func(b map[string]any) { pullReviewPull(b)["reviewThreads"] = nil },
		"null nodes":                func(b map[string]any) { pullReviewConnection(b)["nodes"] = nil },
		"null node":                 func(b map[string]any) { pullReviewConnection(b)["nodes"] = []any{nil} },
		"missing boolean": func(b map[string]any) {
			pullReviewConnection(b)["nodes"] = []any{map[string]any{"id": "T1", "isResolved": false}}
		},
		"negative count": func(b map[string]any) { pullReviewConnection(b)["totalCount"] = -1 },
		"capacity count": func(b map[string]any) { pullReviewConnection(b)["totalCount"] = 10001 },
		"count gap":      func(b map[string]any) { pullReviewConnection(b)["totalCount"] = 2 },
		"duplicate id": func(b map[string]any) {
			pullReviewConnection(b)["totalCount"] = 2
			pullReviewConnection(b)["nodes"] = []any{pullReviewNode("T1", false, false), pullReviewNode("T1", true, true)}
		},
		"empty id":                func(b map[string]any) { pullReviewConnection(b)["nodes"] = []any{pullReviewNode("", false, false)} },
		"null page":               func(b map[string]any) { pullReviewConnection(b)["pageInfo"] = nil },
		"missing nullable cursor": func(b map[string]any) { delete(pullReviewConnection(b)["pageInfo"].(map[string]any), "endCursor") },
		"missing cursor":          func(b map[string]any) { pullReviewConnection(b)["pageInfo"].(map[string]any)["endCursor"] = nil },
		"empty cursor":            func(b map[string]any) { pullReviewConnection(b)["pageInfo"].(map[string]any)["endCursor"] = "" },
		"initial target mismatch": func(b map[string]any) { pullReviewPull(b)["headRefOid"] = strings.Repeat("c", 40) },
		"case alias":              func(b map[string]any) { pullReviewPull(b)["State"] = "OPEN" },
		"numeric bool":            func(b map[string]any) { pullReviewPull(b)["isDraft"] = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				b := pullReviewPageFixture(1, []any{pullReviewNode("T1", false, false)}, false, "c1")
				mutate(b)
				_ = json.NewEncoder(w).Encode(b)
			}))
			defer s.Close()
			got, err := testGitHub(s, "synthetic-secret").ObservePullReviewState(t.Context(), pullReviewTarget())
			if !hasCode(err, "provider_invalid_response") || got.State == "complete" || calls.Load() != 1 || strings.Contains(fmt.Sprint(err), "synthetic-secret") || strings.Contains(fmt.Sprint(err), "private provider") {
				t.Fatal(got, err, calls.Load())
			}
		})
	}
}

func TestPullReviewStatePaginationRejectsIncompleteCollections(t *testing.T) {
	for _, kind := range []string{"duplicate across pages", "repeated cursor", "changed count", "partial error", "metadata movement", "page nodes over100", "empty nonterminal", "count reached with next"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				b := pullReviewPageFixture(2, []any{pullReviewNode("T1", false, false)}, true, "c1")
				if kind == "page nodes over100" {
					nodes := []any{}
					for i := 0; i < 101; i++ {
						nodes = append(nodes, pullReviewNode(fmt.Sprintf("T%d", i), false, false))
					}
					b = pullReviewPageFixture(101, nodes, false, "last")
				}
				if kind == "empty nonterminal" {
					b = pullReviewPageFixture(2, []any{}, true, "c1")
				}
				if kind == "count reached with next" {
					b = pullReviewPageFixture(1, []any{pullReviewNode("T1", false, false)}, true, "c1")
				}
				if n == 2 {
					b = pullReviewPageFixture(2, []any{pullReviewNode("T2", false, false)}, false, "c2")
					switch kind {
					case "duplicate across pages":
						pullReviewConnection(b)["nodes"] = []any{pullReviewNode("T1", false, false)}
					case "repeated cursor":
						pullReviewConnection(b)["pageInfo"].(map[string]any)["endCursor"] = "c1"
					case "changed count":
						pullReviewConnection(b)["totalCount"] = 3
					case "partial error":
						w.WriteHeader(503)
						_, _ = w.Write([]byte("private-error-secret"))
						return
					case "metadata movement":
						pullReviewPull(b)["baseRefOid"] = strings.Repeat("c", 40)
					}
				}
				_ = json.NewEncoder(w).Encode(b)
			}))
			defer s.Close()
			got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget())
			if kind == "metadata movement" {
				if err != nil || got.Reason != "review_state_changed" {
					t.Fatal(got, err)
				}
			} else if err == nil {
				t.Fatal("incomplete inventory accepted", got)
			}
			if got.State == "complete" || got.Threads != nil || calls.Load() > 2 || strings.Contains(fmt.Sprint(err), "private-error-secret") {
				t.Fatal(got, err, calls.Load())
			}
		})
	}
}

func TestPullReviewStateStrictRepositoryFullID(t *testing.T) {
	for _, value := range []any{"4294967296", nil, 4294967296, "04294967296", "+4294967296", "4294967296.0", "9223372036854775808", "0", "-1", " 4294967296"} {
		t.Run(fmt.Sprintf("%T-%v", value, value), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				b := pullReviewPageFixture(0, []any{}, false, nil)
				b["data"].(map[string]any)["repository"].(map[string]any)["fullDatabaseId"] = value
				_ = json.NewEncoder(w).Encode(b)
			}))
			defer s.Close()
			target := pullReviewTarget()
			target.RepositoryID = 4294967296
			got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), target)
			if value == "4294967296" {
				if err != nil || got.State != "complete" || calls.Load() != 2 {
					t.Fatal(got, err, calls.Load())
				}
			} else if !hasCode(err, "provider_invalid_response") || calls.Load() != 1 {
				t.Fatal(got, err, calls.Load())
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := pullReviewPageFixture(0, []any{}, false, nil)
			delete(b["data"].(map[string]any)["repository"].(map[string]any), "fullDatabaseId")
			_ = json.NewEncoder(w).Encode(b)
		}))
		defer s.Close()
		if _, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget()); !hasCode(err, "provider_invalid_response") {
			t.Fatal(err)
		}
	})
}

func TestPullReviewStateNoHTTPForInvalidSelectorsOrCanceledContext(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("invalid selector reached provider")
	}))
	defer s.Close()
	g := testGitHub(s, "")
	for _, change := range []func(*CITarget){func(v *CITarget) { v.Repository = "../escape" }, func(v *CITarget) { v.RepositoryID = 0 }, func(v *CITarget) { v.PullNumber = 0 }, func(v *CITarget) { v.HeadSHA = strings.Repeat("A", 40) }, func(v *CITarget) { v.BaseSHA = "" }, func(v *CITarget) { v.BaseRef = "" }} {
		target := pullReviewTarget()
		change(&target)
		if _, err := g.ObservePullReviewState(t.Context(), target); err == nil {
			t.Fatal("invalid selector admitted")
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.ObservePullReviewState(canceled, pullReviewTarget()); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := g.ObservePullReviewState(nil, pullReviewTarget()); err == nil || calls.Load() != 0 {
		t.Fatal(err, calls.Load())
	}
	var absent *GitHub
	if _, err := absent.ObservePullReviewState(t.Context(), pullReviewTarget()); err == nil {
		t.Fatal("nil adapter panicked or succeeded")
	}
}

func TestPullReviewStateCancellationReleasesHTTPWithoutSecondScan(t *testing.T) {
	entered := make(chan struct{})
	released := make(chan struct{})
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(released)
	}))
	defer s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := testGitHub(s, "synthetic-secret").ObservePullReviewState(ctx, pullReviewTarget())
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || strings.Contains(fmt.Sprint(err), "synthetic-secret") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP did not cancel")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("server request leaked")
	}
	if calls.Load() != 1 {
		t.Fatal("later call after cancellation", calls.Load())
	}
}

func TestPullReviewStateHTTPFailuresPrivacyAndRedirect(t *testing.T) {
	var foreign atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1) }))
	defer destination.Close()
	for _, status := range []int{301, 401, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", destination.URL+"/graphql")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("private-provider-text"))
			}))
			defer s.Close()
			got, err := testGitHub(s, "synthetic-secret").ObservePullReviewState(t.Context(), pullReviewTarget())
			var e *Error
			if !errors.As(err, &e) || e.OutcomeUnknown || got.State == "complete" || calls.Load() != 1 || strings.Contains(fmt.Sprint(err), "private-provider-text") || strings.Contains(fmt.Sprint(err), "synthetic-secret") {
				t.Fatal(got, err, calls.Load())
			}
		})
	}
	if foreign.Load() != 0 {
		t.Fatal("redirect leaked credentials")
	}
}

func TestPullReviewStateRawDuplicatesAliasesAndInvalidJSON(t *testing.T) {
	for index, mutate := range []func(string) string{
		func(s string) string {
			return strings.Replace(s, `"isDraft":false`, `"isDraft":false,"isDraft":true`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"isResolved":false`, `"isResolved":false,"IsResolved":false`, 1)
		},
		func(s string) string { return strings.Replace(s, `"headRefOid":`, `"headRefOID":`, 1) },
		func(s string) string { return s + ` {"extra":true}` },
		func(s string) string { return strings.Replace(s, `"T1"`, `"`+string([]byte{0xff})+`"`, 1) },
	} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := json.Marshal(pullReviewPageFixture(1, []any{pullReviewNode("T1", false, false)}, false, "c1"))
				_, _ = w.Write([]byte(mutate(string(b))))
			}))
			defer s.Close()
			if got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget()); !hasCode(err, "provider_invalid_response") || got.State == "complete" {
				t.Fatal(got, err)
			}
		})
	}
}

func TestPullReviewStateSharedByteAndPageBudgets(t *testing.T) {
	t.Run("aggregate bytes", func(t *testing.T) {
		var calls atomic.Int64
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			nodes := []any{pullReviewNode("T1", false, false)}
			next, cursor := true, "c1"
			if n%2 == 0 {
				nodes = []any{pullReviewNode("T2", false, false)}
				next, cursor = false, "c2"
			}
			b := pullReviewPageFixture(2, nodes, next, cursor)
			b["future"] = strings.Repeat("x", 6<<20)
			_ = json.NewEncoder(w).Encode(b)
		}))
		defer s.Close()
		got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget())
		if !hasCode(err, "provider_invalid_response") || got.State == "complete" || calls.Load() != 3 {
			t.Fatal(got, err, calls.Load())
		}
	})
	t.Run("per body", func(t *testing.T) {
		var calls atomic.Int64
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			b := pullReviewPageFixture(0, []any{}, false, nil)
			b["future"] = strings.Repeat("x", githubMaxBody)
			_ = json.NewEncoder(w).Encode(b)
		}))
		defer s.Close()
		got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget())
		if !hasCode(err, "provider_invalid_response") || got.State == "complete" || calls.Load() != 1 {
			t.Fatal(got, err, calls.Load())
		}
	})
	t.Run("pages across both scans", func(t *testing.T) {
		var calls atomic.Int64
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := int(calls.Add(1))
			page := (n-1)%60 + 1
			_ = json.NewEncoder(w).Encode(pullReviewPageFixture(60, []any{pullReviewNode(fmt.Sprintf("T%d", page), false, false)}, page < 60, fmt.Sprintf("c%d", page)))
		}))
		defer s.Close()
		got, err := testGitHub(s, "").ObservePullReviewState(t.Context(), pullReviewTarget())
		if !hasCode(err, "provider_invalid_response") || got.State == "complete" || calls.Load() != 100 {
			t.Fatal(got, err, calls.Load())
		}
	})
}

func TestPullReviewStateShortParentDeadlineAndBetweenScanCancel(t *testing.T) {
	t.Run("short parent", func(t *testing.T) {
		var calls atomic.Int64
		released := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			close(released)
		}))
		defer s.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
		defer cancel()
		start := time.Now()
		got, err := testGitHub(s, "").ObservePullReviewState(ctx, pullReviewTarget())
		if err == nil || got.State == "complete" || calls.Load() != 1 || time.Since(start) > time.Second {
			t.Fatal(got, err, calls.Load(), time.Since(start))
		}
		select {
		case <-released:
		case <-time.After(time.Second):
			t.Fatal("request leaked")
		}
	})
	t.Run("between scans", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls atomic.Int64
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(pullReviewPageFixture(0, []any{}, false, nil))
			cancel()
		}))
		defer s.Close()
		got, err := testGitHub(s, "").ObservePullReviewState(ctx, pullReviewTarget())
		if err == nil || got.State == "complete" || calls.Load() != 1 {
			t.Fatal(got, err, calls.Load())
		}
	})
}
