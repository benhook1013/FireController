package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCICandidateCohortSameFacts(t *testing.T) {
	for _, mode := range []string{"stable", "two_refs", "one_group_fail", "pending_member", "conflicting_member", "wrong_head_member", "invalid_member", "stale_merge_parent", "reverse_parent", "member_object_fail", "shared_branch_fail", "missing_branch"} {
		t.Run(mode, func(t *testing.T) {
			const n = 8
			head := func(i int) string { return fmt.Sprintf("%040x", 100+i) }
			merge := func(i int) string { return fmt.Sprintf("%040x", 200+i) }
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				switch {
				case strings.Contains(r.URL.Path, "/pulls/"):
					i, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
					p := candidatePullFixture()
					p["number"] = i
					p["head"].(map[string]any)["sha"] = head(i)
					p["merge_commit_sha"] = merge(i)
					if (mode == "two_refs" || mode == "one_group_fail") && i%2 == 0 {
						p["base"].(map[string]any)["ref"] = "stack/parent"
					}
					if i == 3 {
						switch mode {
						case "pending_member":
							p["mergeable"] = nil
						case "conflicting_member":
							p["mergeable"] = false
						case "wrong_head_member":
							p["head"].(map[string]any)["sha"] = head(99)
						case "invalid_member":
							delete(p, "mergeable")
						}
					}
					_ = json.NewEncoder(w).Encode(p)
				case strings.Contains(r.URL.Path, "/branches/"):
					if mode == "shared_branch_fail" || mode == "one_group_fail" && strings.Contains(r.URL.Path, "stack") {
						http.Error(w, "synthetic", 500)
						return
					}
					if mode == "missing_branch" {
						http.NotFound(w, r)
						return
					}
					b := candidateBranchFixture()
					if strings.Contains(r.URL.Path, "stack") {
						b["name"] = "stack/parent"
					}
					_ = json.NewEncoder(w).Encode(b)
				case strings.Contains(r.URL.Path, "/git/commits/"):
					sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					for i := 1; i <= n; i++ {
						if sha == merge(i) {
							if mode == "member_object_fail" && i == 3 {
								http.Error(w, "synthetic", 500)
								return
							}
							base := candidateBase
							if mode == "stale_merge_parent" && i == 3 {
								base = head(99)
							}
							a, b := base, head(i)
							if mode == "reverse_parent" {
								a, b = b, a
							}
							v := candidateCommitFixture(a, b)
							v["sha"] = sha
							_ = json.NewEncoder(w).Encode(v)
							return
						}
					}
					http.NotFound(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			g := testGitHub(server, "synthetic")
			targets := make([]CITarget, n)
			expected := make([]ciCandidateCohortResult, n)
			for i := 1; i <= n; i++ {
				ref := "main"
				if (mode == "two_refs" || mode == "one_group_fail") && i%2 == 0 {
					ref = "stack/parent"
				}
				targets[i-1] = CITarget{Repository: "owner/repo", RepositoryID: 20, PullNumber: int64(i), HeadSHA: head(i), BaseSHA: candidateBase, BaseRef: ref}
				v, err := g.PullCICandidate(t.Context(), "owner/repo", 20, int64(i), head(i))
				expected[i-1] = ciCandidateCohortResult{v, err}
			}
			mu.Lock()
			baselineCalls := calls
			calls = 0
			mu.Unlock()
			// Compare cold candidates on both paths; warm reuse has separate proof.
			g.mergeObjects = immutableMergeCache{}
			got, err := g.pullCICandidateCohort(t.Context(), targets, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != n {
				t.Fatal(got)
			}
			for i := range got {
				got[i].Candidate.ObservedAt = time.Time{}
				expected[i].Candidate.ObservedAt = time.Time{}
				if !reflect.DeepEqual(got[i].Candidate, expected[i].Candidate) {
					t.Fatalf("member%d facts differ\ngot%+v\nwant%+v", i, got[i], expected[i])
				}
				if (got[i].Err == nil) != (expected[i].Err == nil) {
					t.Fatal("error presence", i, got[i].Err, expected[i].Err)
				}
				var a, b *Error
				if errors.As(got[i].Err, &a) && errors.As(expected[i].Err, &b) && (a.Code != b.Code || a.Status != b.Status) {
					t.Fatal("error identity", a, b)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if mode == "stable" && (baselineCalls != 40 || calls != 26) {
				t.Fatal("measured request count", baselineCalls, calls)
			}
			if mode == "two_refs" && (baselineCalls != 40 || calls != 28) {
				t.Fatal("grouping count", baselineCalls, calls)
			}
		})
	}
}

func TestCICandidateCohortCanceledAndInvalidNoProviderCalls(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; t.Error("unexpected provider request") }))
	defer s.Close()
	g := testGitHub(s, "synthetic")
	target := CITarget{Repository: "owner/repo", RepositoryID: 20, PullNumber: 7, HeadSHA: candidateHead, BaseSHA: candidateBase, BaseRef: "main"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := g.pullCICandidateCohort(ctx, []CITarget{target}, nil)
	if err != nil || len(got) != 1 || got[0].Err == nil {
		t.Fatal(got, err)
	}
	if _, err = g.pullCICandidateCohort(t.Context(), []CITarget{target, target}, nil); err == nil {
		t.Fatal("duplicate enrollment accepted")
	}
	if _, err = g.pullCICandidateCohort(t.Context(), make([]CITarget, 9), nil); err == nil {
		t.Fatal("unbounded enrollment")
	}
	if calls != 0 {
		t.Fatal(calls)
	}
}

func TestCICandidateCohortMovementAndMidflightCancellation(t *testing.T) {
	for _, mode := range []string{"base_moves", "one_pr_moves", "cancel_object", "deadline_branch"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "deadline_branch" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
			}
			var mu sync.Mutex
			branches := 0
			pulls := map[int]int{}
			var paths []string
			head := func(i int) string { return fmt.Sprintf("%040x", 100+i) }
			merge := func(i int) string { return fmt.Sprintf("%040x", 200+i) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.URL.Path)
				switch {
				case strings.Contains(r.URL.Path, "/pulls/"):
					i, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
					pulls[i]++
					p := candidatePullFixture()
					p["number"] = i
					p["head"].(map[string]any)["sha"] = head(i)
					p["merge_commit_sha"] = merge(i)
					if mode == "one_pr_moves" && i == 2 && pulls[i] == 2 {
						p["head"].(map[string]any)["sha"] = head(99)
					}
					_ = json.NewEncoder(w).Encode(p)
				case strings.Contains(r.URL.Path, "/branches/"):
					branches++
					if mode == "deadline_branch" {
						<-r.Context().Done()
						return
					}
					b := candidateBranchFixture()
					if mode == "base_moves" && branches == 2 {
						b["commit"].(map[string]any)["sha"] = head(99)
					}
					_ = json.NewEncoder(w).Encode(b)
				case strings.Contains(r.URL.Path, "/git/commits/"):
					sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					for i := 1; i <= 3; i++ {
						if sha == merge(i) {
							if mode == "cancel_object" && i == 2 {
								cancel()
								return
							}
							v := candidateCommitFixture(candidateBase, head(i))
							v["sha"] = sha
							_ = json.NewEncoder(w).Encode(v)
							return
						}
					}
				}
			}))
			defer server.Close()
			targets := make([]CITarget, 3)
			for i := 1; i <= 3; i++ {
				targets[i-1] = CITarget{Repository: "owner/repo", RepositoryID: 20, PullNumber: int64(i), HeadSHA: head(i), BaseSHA: candidateBase, BaseRef: "main"}
			}
			got, err := testGitHub(server, "synthetic").pullCICandidateCohort(ctx, targets, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range got {
				switch mode {
				case "base_moves":
					if v.Err != nil || v.Candidate.State != "unproven" {
						t.Fatal("moving shared base credited", got)
					}
				case "one_pr_moves":
					want := "complete"
					if i == 1 {
						want = "unproven"
					}
					if v.Err != nil || v.Candidate.State != want {
						t.Fatal("moving member contamination", got)
					}
				default:
					if v.Err == nil || v.Candidate.State == "complete" {
						t.Fatal("partial proof survived cancellation", got)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if mode == "cancel_object" && len(paths) != 6 {
				t.Fatal("calls after cancellation", paths)
			}
			if mode == "deadline_branch" && len(paths) != 4 {
				t.Fatal("calls after parent deadline", paths)
			}
		})
	}
}

func TestCICandidateCohortPreservesMemberByteBudgets(t *testing.T) {
	for _, mode := range []string{"member_aggregate", "independent_groups"} {
		t.Run(mode, func(t *testing.T) {
			const branchPadding = 6 << 20
			var mu sync.Mutex
			pullCalls := map[int]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/pulls/"):
					i, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
					mu.Lock()
					pullCalls[i]++
					mu.Unlock()
					p := candidatePullFixture()
					p["number"] = i
					if mode == "independent_groups" {
						p["base"].(map[string]any)["ref"] = fmt.Sprintf("branch%d", i)
					}
					if mode == "member_aggregate" && i == 1 {
						p["padding"] = strings.Repeat("x", 5<<20)
					}
					_ = json.NewEncoder(w).Encode(p)
				case strings.Contains(r.URL.Path, "/branches/"):
					b := candidateBranchFixture()
					b["name"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					b["padding"] = strings.Repeat("x", branchPadding)
					_ = json.NewEncoder(w).Encode(b)
				case strings.Contains(r.URL.Path, "/git/commits/"):
					_ = json.NewEncoder(w).Encode(candidateCommitFixture(candidateBase, candidateHead))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			n := 2
			if mode == "independent_groups" {
				n = 3
			}
			targets := make([]CITarget, n)
			for i := range targets {
				ref := "main"
				if mode == "independent_groups" {
					ref = fmt.Sprintf("branch%d", i+1)
				}
				targets[i] = CITarget{Repository: "owner/repo", RepositoryID: 20, PullNumber: int64(i + 1), HeadSHA: candidateHead, BaseSHA: candidateBase, BaseRef: ref}
			}
			got, err := testGitHub(server, "synthetic").pullCICandidateCohort(t.Context(), targets, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range got {
				if mode == "member_aggregate" && i == 0 {
					var e *Error
					if !errors.As(v.Err, &e) || e.Code != "provider_invalid_response" || v.Candidate.State == "complete" {
						t.Fatal("shared bytes bypassed member bound", v)
					}
				} else if v.Err != nil || v.Candidate.State != "complete" {
					t.Fatal("other group/member byte budget contaminated", i, v)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if mode == "member_aggregate" && pullCalls[1] != 1 {
				t.Fatal("over-budget member made closing read", pullCalls)
			}
		})
	}
}

func TestCICandidateCohortMemberCacheIsolation(t *testing.T) {
	f, targets := cohortFixture(t, "")
	caches := make([]map[string]githubCIMerge, 2)
	var object githubCIMerge
	data, _ := json.Marshal(candidateCommitFixture(candidateBase, candidateHead))
	if err := decodeProviderJSON(data, &object); err != nil {
		t.Fatal(err)
	}
	caches[0] = map[string]githubCIMerge{candidateMerge: object}
	got, err := f.g.pullCICandidateCohort(t.Context(), targets, caches)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range got {
		if v.Err != nil || v.Candidate.State != "complete" {
			t.Fatal(got)
		}
	}
	_, counts := f.snapshot()
	if counts[candidateCommitPath] != 1 {
		t.Fatal("member-local immutable cache not respected", counts)
	}
	if caches[1] != nil {
		t.Fatal("cache implicitly crossed member boundary")
	}
}
