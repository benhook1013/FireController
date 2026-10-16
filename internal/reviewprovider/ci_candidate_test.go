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
const candidateBranchPath = "/repos/owner/repo/branches/main"
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

func candidateBranchFixture() map[string]any {
	return map[string]any{"name": "main", "commit": map[string]any{"sha": candidateBase}}
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
			case candidateBranchPath:
				_ = json.NewEncoder(w).Encode(candidateBranchFixture())
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
		if err != nil || got.State != "complete" || got.Reason != "" || got.RepositoryID != 20 || got.Repository != "owner/repo" || got.PullNumber != 7 || got.HeadSHA != candidateHead || got.BaseSHA != candidateBase || got.MergeBaseParentSHA != candidateBase || got.BaseRef != "main" || got.MergeSHA != candidateMerge || got.MergeState != "verified" || got.ObservedAt.Before(before) || got.ObservedAt.After(time.Now().UTC()) || got.ObservedAt.Location() != time.UTC {
			t.Fatal("candidate identity or parent set lost", got, err)
		}
		mu.Lock()
		if len(paths) != 5 || paths[0] != candidatePullPath || paths[1] != candidateBranchPath || paths[2] != candidateCommitPath || paths[3] != candidateBranchPath || paths[4] != candidatePullPath {
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
					if r.URL.Path == candidateBranchPath {
						_ = json.NewEncoder(w).Encode(candidateBranchFixture())
						return
					}
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
					want, count = "ci_candidate_changed", 5
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
				if r.URL.Path == candidateBranchPath {
					_ = json.NewEncoder(w).Encode(candidateBranchFixture())
					return
				}
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
			if r.URL.Path == candidateBranchPath {
				_ = json.NewEncoder(w).Encode(candidateBranchFixture())
				return
			}
			var body any = candidatePullFixture()
			if r.URL.Path == candidateCommitPath {
				body = candidateCommitFixture(parents...)
			}
			_ = json.NewEncoder(w).Encode(body)
		}))
		got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
		server.Close()
		if err != nil || got.State != "unproven" || got.Reason != "ci_merge_identity_unproven" || got.MergeSHA != "" || calls.Load() != 5 {
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
		case candidateBranchPath:
			branch := candidateBranchFixture()
			branch["commit"].(map[string]any)["sha"] = candidateHead
			body = branch
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
	if err != nil || got.State != "unproven" || got.Reason != "ci_merge_identity_unproven" || got.MergeState != "unproven" || got.MergeSHA != "" || calls.Load() != 5 {
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
				if r.URL.Path == candidateBranchPath {
					stage = "branch"
					body = candidateBranchFixture()
				}
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
				count = 3
			}
			if test.stage == "final" {
				count = 5
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
				if r.URL.Path == candidateBranchPath {
					_ = json.NewEncoder(w).Encode(candidateBranchFixture())
					return
				}
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
				want, count = "ci_merge_identity_unproven", 5
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
		case candidateBranchPath:
			body = candidateBranchFixture()
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
	if err != nil || got.State != "complete" || got.Repository != "owner/repo" || got.RepositoryID != 20 || calls.Load() != 5 {
		t.Fatal("canonical repository case binding failed", got, err, calls.Load())
	}
}

func TestPullCICandidateTransportFailureAndStrictJSON(t *testing.T) {
	for _, stage := range []int64{1, 2, 3, 4, 5} {
		for _, mode := range []string{"unauthorized", "forbidden", "not_found", "unavailable", "redirect", "duplicate", "alias"} {
			t.Run(string(rune('0'+stage))+"/"+mode, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					if r.Method != "GET" || r.URL.RawQuery != "" {
						t.Error("changed candidate method or selectors")
					}
					if r.URL.Path != candidatePullPath && r.URL.Path != candidateCommitPath && r.URL.Path != candidateBranchPath {
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
							if r.URL.Path == candidateCommitPath {
								p = candidateCommitFixture(candidateBase, candidateHead)
								p["SHA"] = p["sha"]
								delete(p, "sha")
							}
							if r.URL.Path == candidateBranchPath {
								p = candidateBranchFixture()
								p["Name"] = p["name"]
								delete(p, "name")
							}
							_ = json.NewEncoder(w).Encode(p)
						}
						return
					}
					body := candidatePullFixture()
					if r.URL.Path == candidateBranchPath {
						body = candidateBranchFixture()
					}
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
				expectedError := hasCode(err, code)
				if mode == "not_found" && (stage == 2 || stage == 4) {
					expectedError = err == nil
				}
				wantCalls := stage
				if mode == "not_found" && stage == 4 {
					wantCalls = 5
				} // Preserve the final PR reread after an unknown closing branch.
				if !expectedError || got.State != "unproven" || got.MergeSHA != "" || got.MergeBaseParentSHA != "" || calls.Load() != wantCalls {
					t.Fatal("failed candidate read granted proof or retried", got, err, calls.Load())
				}
				if err != nil && (strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "private.invalid")) {
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
		if r.URL.Path == candidateBranchPath {
			_ = json.NewEncoder(w).Encode(candidateBranchFixture())
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
	if err != nil || got.State != "complete" || len(deadlines) != 5 {
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
	if !hasCode(err, "provider_unavailable") || got.State != "unproven" || got.MergeSHA != "" || calls.Load() != 8 {
		t.Fatal("cancelled candidate advanced or retried", got, err, calls.Load())
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancel left candidate HTTP pending")
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	if _, err = g.PullCICandidate(stopped, "owner/repo", 20, 7, candidateHead); err == nil || calls.Load() != 8 {
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
			if r.URL.Path == candidateBranchPath {
				body = candidateBranchFixture()
			}
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

func TestPullCICandidateStalePullBaseUsesIndependentBranch(t *testing.T) {
	const tip = "dddddddddddddddddddddddddddddddddddddddd"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Error("unscoped candidate request")
		}
		switch r.URL.Path {
		case candidatePullPath:
			_ = json.NewEncoder(w).Encode(candidatePullFixture())
		case "/repos/owner/repo/branches/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": tip}})
		case candidateCommitPath:
			_ = json.NewEncoder(w).Encode(candidateCommitFixture(candidateHead, tip))
		default:
			t.Error("unselected candidate resource", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
	encoded, _ := json.Marshal(got)
	var wire map[string]any
	_ = json.Unmarshal(encoded, &wire)
	if err != nil || got.State != "complete" || got.MergeState != "verified" || got.BaseSHA != candidateBase || got.MergeBaseParentSHA != tip || wire["merge_base_parent_sha"] != tip {
		t.Fatal("stale PR base did not retain its separate verified live base parent", got, err, wire)
	}
	want := []string{candidatePullPath, "/repos/owner/repo/branches/main", candidateCommitPath, "/repos/owner/repo/branches/main", candidatePullPath}
	if len(paths) != len(want) {
		t.Fatal("candidate read scope/order", paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatal("candidate read scope/order", paths)
		}
	}
}

func TestPullCICandidateBranchIdentityAndShape(t *testing.T) {
	for _, closing := range []bool{false, true} {
		for _, mode := range []string{"wrong_name", "moving_tip", "old_base_parent", "missing_name", "null_name", "missing_commit", "null_commit", "missing_sha", "null_sha", "uppercase_sha", "bad_sha"} {
			t.Run(map[bool]string{false: "opening", true: "closing"}[closing]+"/"+mode, func(t *testing.T) {
				var calls, branches atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodGet || r.URL.RawQuery != "" {
						t.Error("unscoped candidate request")
					}
					body := candidatePullFixture()
					switch r.URL.Path {
					case candidatePullPath:
					case candidateCommitPath:
						body = candidateCommitFixture(candidateBase, candidateHead)
					case candidateBranchPath:
						body = candidateBranchFixture()
						n := branches.Add(1)
						if mode == "old_base_parent" {
							body["commit"].(map[string]any)["sha"] = strings.Repeat("d", 40)
						}
						if closing && n == 2 || !closing && n == 1 {
							switch mode {
							case "wrong_name":
								body["name"] = "foreign"
							case "moving_tip":
								body["commit"].(map[string]any)["sha"] = strings.Repeat("d", 40)
							case "missing_name":
								delete(body, "name")
							case "null_name":
								body["name"] = nil
							case "missing_commit":
								delete(body, "commit")
							case "null_commit":
								body["commit"] = nil
							case "missing_sha":
								delete(body["commit"].(map[string]any), "sha")
							case "null_sha":
								body["commit"].(map[string]any)["sha"] = nil
							case "uppercase_sha":
								body["commit"].(map[string]any)["sha"] = strings.Repeat("B", 40)
							case "bad_sha":
								body["commit"].(map[string]any)["sha"] = "PRIVATE_BAD_SHA"
							}
						}
					default:
						t.Error("unselected candidate resource", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
				malformed := mode != "wrong_name" && mode != "moving_tip" && mode != "old_base_parent"
				wantCalls := int64(5)
				if !closing && (malformed || mode == "wrong_name") {
					wantCalls = 2
				} else if closing && malformed {
					wantCalls = 4
				}
				if got.State != "unproven" || got.MergeSHA != "" || got.MergeBaseParentSHA != "" || got.Reason != "ci_merge_identity_unproven" || calls.Load() != wantCalls || malformed && !hasCode(err, "provider_invalid_response") || !malformed && err != nil {
					t.Fatal("unbound branch granted proof or malformed branch changed failure", got, err, calls.Load())
				}
				if err != nil && strings.Contains(err.Error(), "PRIVATE") {
					t.Fatal("private branch data leaked", err)
				}
			})
		}
	}
}

func TestPullCICandidateEscapesOnlySelectedBaseBranch(t *testing.T) {
	const ref = "release/v2+fix%25"
	const escaped = "/repos/owner/repo/branches/release%2Fv2+fix%2525"
	var branches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Error("candidate escaped into another selector")
		}
		body := candidatePullFixture()
		switch r.URL.EscapedPath() {
		case candidatePullPath:
			body["base"].(map[string]any)["ref"] = ref
		case candidateCommitPath:
			body = candidateCommitFixture(candidateBase, candidateHead)
		case escaped:
			branches.Add(1)
			body = candidateBranchFixture()
			body["name"] = ref
		default:
			t.Error("escaped branch changed resource", r.URL.EscapedPath())
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	got, err := testGitHub(server, "").PullCICandidate(t.Context(), "owner/repo", 20, 7, candidateHead)
	if err != nil || got.State != "complete" || got.BaseRef != ref || got.MergeBaseParentSHA != candidateBase || branches.Load() != 2 {
		t.Fatal("selected branch was not independently bound", got, err, branches.Load())
	}
}

func TestPullCICandidateBranchCancellationStopsSharedRead(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(map[bool]string{false: "opening", true: "closing"}[closing], func(t *testing.T) {
			var calls, branches atomic.Int64
			entered := make(chan struct{})
			stopped := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body := candidatePullFixture()
				switch r.URL.Path {
				case candidatePullPath:
				case candidateCommitPath:
					body = candidateCommitFixture(candidateBase, candidateHead)
				case candidateBranchPath:
					n := branches.Add(1)
					if !closing || n == 2 {
						close(entered)
						<-r.Context().Done()
						close(stopped)
						return
					}
					body = candidateBranchFixture()
				default:
					t.Error("unselected resource", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			var got CICandidate
			var err error
			go func() {
				got, err = testGitHub(server, "").PullCICandidate(ctx, "owner/repo", 20, 7, candidateHead)
				close(done)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("branch read did not start")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancelled branch did not return")
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("cancelled branch HTTP survived")
			}
			wantCalls := int64(2)
			if closing {
				wantCalls = 4
			}
			if !hasCode(err, "provider_unavailable") || got.State != "unproven" || got.MergeSHA != "" || got.MergeBaseParentSHA != "" || calls.Load() != wantCalls {
				t.Fatal("cancelled branch advanced or retried", got, err, calls.Load())
			}
		})
	}
}
