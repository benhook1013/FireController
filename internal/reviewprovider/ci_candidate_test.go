package reviewprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const candidateHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const candidateBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const candidateMerge = "cccccccccccccccccccccccccccccccccccccccc"
const candidatePullPath = "/repos/owner/repo/pulls/7"
const candidateCommitPath = "/repos/owner/repo/git/commits/" + candidateMerge

func candidatePullFixture() map[string]any {
	return map[string]any{
		"title": "PRIVATE_TITLE", "number": 7, "state": "open", "merged": false,
		"head":          map[string]any{"sha": candidateHead, "repo": map[string]any{"id": 99, "full_name": "fork/repo", "default_branch": "feature"}},
		"base":          map[string]any{"sha": candidateBase, "ref": "main", "repo": map[string]any{"id": 20, "full_name": "owner/repo", "default_branch": "main"}},
		"changed_files": 2, "mergeable": true, "merge_commit_sha": candidateMerge,
	}
}

func candidateCommitFixture(parents ...string) map[string]any {
	rows := make([]map[string]any, 0, len(parents))
	for _, sha := range parents {
		rows = append(rows, map[string]any{"sha": sha})
	}
	return map[string]any{"sha": candidateMerge, "parents": rows, "message": "PRIVATE_COMMIT_MESSAGE"}
}

func TestPullCICandidateVerifiedParentSetAndForkIdentity(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		var paths []string
		var mu sync.Mutex
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			if r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer synthetic-secret" {
				t.Error("candidate read changed method, selectors or credential")
			}
			switch r.URL.Path {
			case candidatePullPath:
				_ = json.NewEncoder(w).Encode(candidatePullFixture())
			case candidateCommitPath:
				parents := []string{candidateBase, candidateHead}
				if reverse {
					parents[0], parents[1] = parents[1], parents[0]
				}
				_ = json.NewEncoder(w).Encode(candidateCommitFixture(parents...))
			default:
				t.Error("unrelated resource", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		before := time.Now().UTC()
		got, err := testGitHub(server, "synthetic-secret").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
		server.Close()
		if err != nil || got.State != "complete" || got.Reason != "" || got.RepositoryID != 20 || got.Repository != "owner/repo" || got.PullNumber != 7 || got.HeadSHA != candidateHead || got.BaseSHA != candidateBase || got.BaseRef != "main" || got.MergeSHA != candidateMerge || got.MergeState != "verified" || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now().UTC()) || got.ObservedAt.Location() != time.UTC {
			t.Fatal("candidate identity or parent set lost", got, err)
		}
		mu.Lock()
		if len(paths) != 3 || paths[0] != candidatePullPath || paths[1] != candidateCommitPath || paths[2] != candidatePullPath {
			t.Fatal("candidate observation order changed", paths)
		}
		mu.Unlock()
		raw, _ := json.Marshal(got)
		for _, private := range []string{"PRIVATE_TITLE", "PRIVATE_COMMIT_MESSAGE", "synthetic-secret", "fork/repo", "merge_ready", "ci_pass"} {
			if strings.Contains(string(raw), private) {
				t.Fatal("candidate proof leaked private facts or invented verdict", string(raw))
			}
		}
	}
}

func TestPullCICandidatePendingAndConflictingAlwaysRecheck(t *testing.T) {
	for _, mode := range []string{"pending", "pending_with_sha", "conflicting", "conflicting_with_sha"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != candidatePullPath {
					t.Error("pending/conflict queried a merge object")
				}
				pull := candidatePullFixture()
				pull["mergeable"] = nil
				if strings.HasPrefix(mode, "conflicting") {
					pull["mergeable"] = false
				}
				if !strings.HasSuffix(mode, "_with_sha") {
					pull["merge_commit_sha"] = nil
				}
				_ = json.NewEncoder(w).Encode(pull)
			}))
			defer server.Close()
			got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
			wantState, wantMerge := "unproven", "pending"
			if strings.HasPrefix(mode, "conflicting") {
				wantState, wantMerge = "complete", "conflicting"
			}
			if err != nil || got.State != wantState || got.MergeState != wantMerge || got.MergeSHA != "" || got.BaseSHA != candidateBase || calls.Load() != 2 {
				t.Fatal("unknown/conflicting merge became verified or skipped reread", got, err, calls.Load())
			}
		})
	}
}

func TestPullCICandidateValidTargetMovement(t *testing.T) {
	changes := map[string]func(map[string]any){
		"repository_id": func(p map[string]any) { p["base"].(map[string]any)["repo"].(map[string]any)["id"] = 21 },
		"repository_name": func(p map[string]any) {
			p["base"].(map[string]any)["repo"].(map[string]any)["full_name"] = "foreign/repo"
		},
		"head":   func(p map[string]any) { p["head"].(map[string]any)["sha"] = strings.Repeat("d", 40) },
		"closed": func(p map[string]any) { p["state"] = "closed" },
		"merged": func(p map[string]any) { p["state"], p["merged"] = "closed", true },
	}
	for _, stage := range []string{"first", "final"} {
		for name, change := range changes {
			t.Run(stage+"/"+name, func(t *testing.T) {
				var calls, pulls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body any = candidateCommitFixture(candidateBase, candidateHead)
					if r.URL.Path == candidatePullPath {
						p := candidatePullFixture()
						n := pulls.Add(1)
						if stage == "first" || n == 2 {
							change(p)
						}
						body = p
					} else if r.URL.Path != candidateCommitPath {
						t.Error("unexpected path", r.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
				want, count := "ci_target_changed", int64(1)
				if stage == "final" {
					want, count = "ci_candidate_changed", 3
				}
				if err != nil || got.State != "unproven" || got.Reason != want || got.MergeSHA != "" || calls.Load() != count {
					t.Fatal("changed target granted candidate proof", got, err, calls.Load())
				}
			})
		}
	}
	for _, mode := range []string{"base", "base_ref", "merge_sha", "merge_null", "mergeable_null", "mergeable_false"} {
		t.Run("final/"+mode, func(t *testing.T) {
			var pulls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any = candidateCommitFixture(candidateBase, candidateHead)
				if r.URL.Path == candidatePullPath {
					p := candidatePullFixture()
					if pulls.Add(1) == 2 {
						switch mode {
						case "base":
							p["base"].(map[string]any)["sha"] = strings.Repeat("d", 40)
						case "base_ref":
							p["base"].(map[string]any)["ref"] = "release"
						case "merge_sha":
							p["merge_commit_sha"] = strings.Repeat("d", 40)
						case "merge_null":
							p["merge_commit_sha"] = nil
						case "mergeable_null":
							p["mergeable"] = nil
						case "mergeable_false":
							p["mergeable"] = false
						}
					}
					body = p
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
			if err != nil || got.State != "unproven" || got.Reason != "ci_candidate_changed" || got.MergeSHA != "" {
				t.Fatal("mixed candidate facts granted proof", got, err)
			}
		})
	}
}

func TestPullCICandidateWrongMergeParentsNeverVerify(t *testing.T) {
	for _, parents := range [][]string{nil, {candidateHead}, {candidateHead, candidateHead}, {candidateBase, strings.Repeat("d", 40)}, {candidateBase, candidateHead, strings.Repeat("d", 40)}} {
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			var body any = candidatePullFixture()
			if r.URL.Path == candidateCommitPath {
				body = candidateCommitFixture(parents...)
			}
			_ = json.NewEncoder(w).Encode(body)
		}))
		got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
		server.Close()
		if err != nil || got.State != "unproven" || got.Reason != "ci_merge_identity_unproven" || got.MergeSHA != "" || calls.Load() != 3 {
			t.Fatal("unbound parent inventory verified", got, err, calls.Load())
		}
	}
}

func TestPullCICandidateEqualHeadBaseCannotVerifyDuplicateParents(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body any
		switch r.URL.Path {
		case candidatePullPath:
			pull := candidatePullFixture()
			pull["base"].(map[string]any)["sha"] = candidateHead
			body = pull
		case candidateCommitPath:
			body = candidateCommitFixture(candidateHead, candidateHead)
		default:
			t.Error("unselected resource", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
	if err != nil || got.State != "unproven" || got.Reason != "ci_merge_identity_unproven" || got.MergeState != "unproven" || got.MergeSHA != "" || calls.Load() != 3 {
		t.Fatal("duplicate parents granted merge proof or skipped final PR read", got, err, calls.Load())
	}
}

func TestPullCICandidateMalformedFieldsFailClosed(t *testing.T) {
	cases := map[string]struct {
		stage string
		edit  func(map[string]any)
	}{
		"missing mergeable":       {"first", func(p map[string]any) { delete(p, "mergeable") }},
		"missing merge sha":       {"first", func(p map[string]any) { delete(p, "merge_commit_sha") }},
		"mergeable alias":         {"first", func(p map[string]any) { p["Mergeable"] = p["mergeable"]; delete(p, "mergeable") }},
		"mergeable string":        {"first", func(p map[string]any) { p["mergeable"] = "true" }},
		"merge sha type":          {"first", func(p map[string]any) { p["merge_commit_sha"] = 3 }},
		"merge sha empty":         {"first", func(p map[string]any) { p["merge_commit_sha"] = "" }},
		"merge sha uppercase":     {"first", func(p map[string]any) { p["merge_commit_sha"] = strings.Repeat("C", 40) }},
		"merge sha null":          {"first", func(p map[string]any) { p["merge_commit_sha"] = nil }},
		"merge equals head":       {"first", func(p map[string]any) { p["merge_commit_sha"] = candidateHead }},
		"merge equals base":       {"first", func(p map[string]any) { p["merge_commit_sha"] = candidateBase }},
		"common alias":            {"first", func(p map[string]any) { p["Number"] = p["number"]; delete(p, "number") }},
		"common missing":          {"first", func(p map[string]any) { delete(p, "changed_files") }},
		"wrong number":            {"first", func(p map[string]any) { p["number"] = 8 }},
		"invalid lifecycle":       {"first", func(p map[string]any) { p["state"] = "future" }},
		"base id zero":            {"first", func(p map[string]any) { p["base"].(map[string]any)["repo"].(map[string]any)["id"] = 0 }},
		"commit sha malformed":    {"commit", func(p map[string]any) { p["sha"] = "PRIVATE_BAD_SHA" }},
		"parent sha malformed":    {"commit", func(p map[string]any) { p["parents"].([]map[string]any)[0]["sha"] = "bad" }},
		"parents null":            {"commit", func(p map[string]any) { p["parents"] = nil }},
		"parents missing":         {"commit", func(p map[string]any) { delete(p, "parents") }},
		"parent field alias":      {"commit", func(p map[string]any) { p["parents"].([]map[string]any)[0] = map[string]any{"SHA": candidateBase} }},
		"final missing mergeable": {"final", func(p map[string]any) { delete(p, "mergeable") }},
		"final bad common field":  {"final", func(p map[string]any) { p["merged"] = "false" }},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			var calls, pulls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				stage := "commit"
				body := candidateCommitFixture(candidateBase, candidateHead)
				if r.URL.Path == candidatePullPath {
					body = candidatePullFixture()
					stage = "first"
					if pulls.Add(1) == 2 {
						stage = "final"
					}
				}
				if stage == test.stage {
					test.edit(body)
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "synthetic-secret").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
			count := int64(1)
			if test.stage == "commit" {
				count = 2
			}
			if test.stage == "final" {
				count = 3
			}
			if !hasCode(err, "provider_invalid_response") || got.State != "unproven" || got.MergeSHA != "" || calls.Load() != count {
				t.Fatal("malformed candidate accepted or continued reads", got, err, calls.Load())
			}
			if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("provider diagnostics leaked", err)
			}
		})
	}
}

func TestPullCICandidatePendingConflictMovementAndWrongCommitIdentity(t *testing.T) {
	for _, mode := range []string{"pending_changed", "conflict_changed", "wrong_commit_sha"} {
		t.Run(mode, func(t *testing.T) {
			var calls, pulls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body := candidateCommitFixture(candidateBase, candidateHead)
				if r.URL.Path == candidatePullPath {
					body = candidatePullFixture()
					n := pulls.Add(1)
					if mode != "wrong_commit_sha" {
						body["merge_commit_sha"] = nil
						body["mergeable"] = nil
						if mode == "conflict_changed" {
							body["mergeable"] = false
						}
						if n == 2 {
							body["base"].(map[string]any)["sha"] = strings.Repeat("d", 40)
						}
					}
				} else if r.URL.Path == candidateCommitPath {
					body["sha"] = strings.Repeat("d", 40)
				} else {
					t.Error("unexpected read", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
			want, count := "ci_candidate_changed", int64(2)
			if mode == "wrong_commit_sha" {
				want, count = "ci_merge_identity_unproven", 3
			}
			if err != nil || got.State != "unproven" || got.Reason != want || got.MergeSHA != "" || calls.Load() != count {
				t.Fatal("changed or unbound merge candidate accepted", got, err, calls.Load())
			}
		})
	}
}

func TestPullCICandidateRejectsInvalidSelectorsBeforeHTTPAndAcceptsRepositoryCase(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body any = candidatePullFixture()
		switch strings.ToLower(r.URL.Path) {
		case candidatePullPath:
		case candidateCommitPath:
			body = candidateCommitFixture(candidateHead, candidateBase)
		default:
			t.Error("unselected request", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	g := testGitHub(server, "")
	for _, input := range []struct {
		ctx    context.Context
		repo   string
		id, pr int64
		head   string
	}{
		{nil, "owner/repo", 20, 7, candidateHead},
		{t.Context(), "../repo", 20, 7, candidateHead},
		{t.Context(), "owner/repo", 0, 7, candidateHead},
		{t.Context(), "owner/repo", 20, 0, candidateHead},
		{t.Context(), "owner/repo", 20, 2147483648, candidateHead},
		{t.Context(), "owner/repo", 20, 7, strings.Repeat("A", 40)},
		{t.Context(), "owner/repo", 20, 7, "HEAD"},
	} {
		if _, err := g.PullCICandidate(input.ctx, input.repo, input.id, input.pr, input.head); err == nil {
			t.Fatal("invalid selector accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selector reached provider", calls.Load())
	}
	var missing *GitHub
	if _, err := missing.PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead); err == nil {
		t.Fatal("nil adapter accepted")
	}
	got, err := g.PullCICandidate(t.Context(), "OWNER/REPO", 20, 7, candidateHead)
	if err != nil || got.State != "complete" || got.Repository != "owner/repo" || got.RepositoryID != 20 || calls.Load() != 3 {
		t.Fatal("canonical repository case binding failed", got, err, calls.Load())
	}
}

func TestPullCICandidateTransportFailureAndStrictJSON(t *testing.T) {
	for _, stage := range []int64{1, 2, 3} {
		for _, mode := range []string{"unauthorized", "forbidden", "not_found", "unavailable", "redirect", "duplicate", "alias"} {
			t.Run(string(rune('0'+stage))+"/"+mode, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					if r.Method != "GET" || r.URL.RawQuery != "" {
						t.Error("changed candidate method or selectors")
					}
					if r.URL.Path != candidatePullPath && r.URL.Path != candidateCommitPath {
						t.Error("foreign endpoint requested", r.URL.Path)
					}
					if n == stage {
						switch mode {
						case "unauthorized":
							w.WriteHeader(401)
						case "forbidden":
							w.WriteHeader(403)
						case "not_found":
							w.WriteHeader(404)
						case "unavailable":
							w.WriteHeader(503)
						case "redirect":
							w.Header().Set("Location", "https://private.invalid/other")
							w.WriteHeader(302)
						case "duplicate":
							_, _ = w.Write([]byte(`{"sha":"` + candidateMerge + `","sha":"` + candidateMerge + `"}`))
						case "alias":
							p := candidatePullFixture()
							p["Merge_commit_sha"] = p["merge_commit_sha"]
							delete(p, "merge_commit_sha")
							if stage == 2 {
								p = candidateCommitFixture(candidateBase, candidateHead)
								p["SHA"] = p["sha"]
								delete(p, "sha")
							}
							_ = json.NewEncoder(w).Encode(p)
						}
						return
					}
					body := candidatePullFixture()
					if r.URL.Path == candidateCommitPath {
						body = candidateCommitFixture(candidateBase, candidateHead)
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				got, err := testGitHub(server, "synthetic-secret").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
				code := "provider_unavailable"
				if mode == "unauthorized" || mode == "forbidden" {
					code = "provider_unauthorized"
				}
				if mode == "not_found" {
					code = "provider_not_found"
				}
				if mode == "duplicate" || mode == "alias" {
					code = "provider_invalid_response"
				}
				if !hasCode(err, code) || got.State != "unproven" || got.MergeSHA != "" || calls.Load() != stage {
					t.Fatal("failed candidate read granted proof or retried", got, err, calls.Load())
				}
				if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "private.invalid") {
					t.Fatal("private diagnostics leaked", err)
				}
			})
		}
	}
}

func TestPullCICandidateSharedDeadlineAndCancellation(t *testing.T) {
	var calls atomic.Int64
	var block atomic.Bool
	closed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if block.Load() && r.URL.Path == candidateCommitPath {
			<-r.Context().Done()
			closed <- struct{}{}
			return
		}
		body := candidatePullFixture()
		if r.URL.Path == candidateCommitPath {
			body = candidateCommitFixture(candidateBase, candidateHead)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	g := testGitHub(server, "")
	g.httpClient.Timeout = 0 // Observe the aggregate context independently of the existing per-request client cap.
	underlying := g.httpClient.Transport
	var deadlines []time.Time
	g.httpClient.Transport = actionsRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("aggregate context missing")
		}
		deadlines = append(deadlines, deadline)
		return underlying.RoundTrip(r)
	})
	before := time.Now()
	got, err := g.PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
	if err != nil || got.State != "complete" || len(deadlines) != 3 {
		t.Fatal("candidate observation failed", got, err, deadlines)
	}
	for _, d := range deadlines {
		if !d.Equal(deadlines[0]) || d.Before(before.Add(14*time.Second)) || d.After(time.Now().Add(15*time.Second)) {
			t.Fatal("candidate request reset aggregate deadline", deadlines)
		}
	}
	block.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	got, err = g.PullCICandidate(ctx, "owner/repo", 20, 7, candidateHead)
	if !hasCode(err, "provider_unavailable") || got.State != "unproven" || got.MergeSHA != "" || calls.Load() != 5 {
		t.Fatal("cancelled candidate advanced or retried", got, err, calls.Load())
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancel left candidate HTTP pending")
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	if _, err = g.PullCICandidate(stopped, "owner/repo", 20, 7, candidateHead); err == nil || calls.Load() != 5 {
		t.Fatal("cancelled parent made HTTP request", err, calls.Load())
	}
}

func TestPullCICandidateSharedByteBudget(t *testing.T) {
	for _, size := range []int{6 << 20, 9 << 20} {
		var calls atomic.Int64
		padding := strings.Repeat("x", size)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			body := candidatePullFixture()
			if r.URL.Path == candidateCommitPath {
				body = candidateCommitFixture(candidateBase, candidateHead)
			}
			body["future_metadata"] = padding
			_ = json.NewEncoder(w).Encode(body)
		}))
		got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
		server.Close()
		wantCalls := int64(3)
		if size > githubMaxBody {
			wantCalls = 1
		}
		if !hasCode(err, "provider_invalid_response") || got.State != "unproven" || got.MergeSHA != "" || calls.Load() != wantCalls {
			t.Fatal("response/aggregate byte limit granted candidate proof", got, err, calls.Load())
		}
	}
}
