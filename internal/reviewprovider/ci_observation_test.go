package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type ciObservationFixture struct {
	t         *testing.T
	g         *GitHub
	mode      string
	mu        sync.Mutex
	paths     []string
	counts    map[string]int
	headWalks int
}

func newCIObservationFixture(t *testing.T, mode string) *ciObservationFixture {
	t.Helper()
	f := &ciObservationFixture{t: t, mode: mode, counts: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.g = testGitHub(server, "synthetic-ci-observation-token")
	return f
}
func ciObservationTarget() CITarget {
	return CITarget{RepositoryID: 20, Repository: "owner/repo", PullNumber: 7, HeadSHA: candidateHead, BaseSHA: candidateBase, BaseRef: "main"}
}
func (f *ciObservationFixture) snapshot() ([]string, map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := append([]string(nil), f.paths...)
	counts := make(map[string]int, len(f.counts))
	for k, v := range f.counts {
		counts[k] = v
	}
	return paths, counts
}
func (f *ciObservationFixture) run(id int64, head string) map[string]any {
	r := actionsRunFixture()
	r["head_sha"] = head
	if id == 60 {
		r["id"], r["check_suite_id"], r["workflow_id"], r["run_number"] = int64(60), int64(41), int64(31), int64(1)
	}
	if id == 51 {
		r["id"], r["check_suite_id"], r["run_number"], r["run_attempt"], r["status"], r["conclusion"] = int64(51), int64(42), int64(8), int64(1), "queued", nil
	}
	return r
}
func (f *ciObservationFixture) checks(head string, call int) []map[string]any {
	if head == candidateMerge && (f.mode == "fallback" || f.mode == "move_empty_merge") && !(f.mode == "move_empty_merge" && call > 1) {
		return []map[string]any{}
	}
	check := ciCheckFixture(1070)
	check["name"], check["head_sha"], check["check_suite"] = "build", head, map[string]any{"id": 40}
	if f.mode == "wrong_app" {
		check["app"] = map[string]any{"id": 11}
	}
	if f.mode == "unlinked_suite" {
		check["check_suite"] = map[string]any{"id": 99}
	}
	if f.mode == "move_results" && call > 1 {
		check["conclusion"] = "failure"
	}
	rows := []map[string]any{check}
	if f.mode == "newer_pending" {
		pending := ciCheckFixture(1072)
		pending["name"], pending["head_sha"], pending["check_suite"], pending["status"], pending["conclusion"], pending["started_at"], pending["completed_at"] = "build", head, map[string]any{"id": 42}, "queued", nil, nil, nil
		rows = append(rows, pending)
	}
	if f.mode == "workflow_capacity" {
		rows = []map[string]any{}
		for i := 0; i < 17; i++ {
			check := ciCheckFixture(int64(1070 + i))
			check["name"], check["head_sha"], check["check_suite"] = fmt.Sprintf("build-%d", i), head, map[string]any{"id": 40 + i}
			rows = append(rows, check)
		}
	}
	return rows
}
func (f *ciObservationFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.RequestURI())
	f.counts[r.URL.Path]++
	call := f.counts[r.URL.Path]
	if r.URL.Path == actionsRunsPath && r.URL.Query().Get("check_suite_id") == "" {
		f.headWalks++
	}
	headWalk := f.headWalks
	f.mu.Unlock()
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-ci-observation-token" {
		f.t.Error("unrelated mutation or credential binding", r.Method, r.URL.Path)
	}
	if f.mode == "deadline" {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(40 * time.Millisecond):
		}
	}
	var body any
	switch {
	case r.URL.Path == "/repos/owner/repo/branches/main/protection":
		checks := []map[string]any{{"context": "build", "app_id": 10}}
		if f.mode == "empty" || f.mode == "empty_moved" {
			body = map[string]any{"required_status_checks": nil}
			break
		}
		if f.mode == "move_authority" && call > 1 {
			checks[0]["context"] = "changed-required-context"
		}
		if f.mode == "move_status_order" {
			checks[0]["app_id"] = -1
		}
		if f.mode == "context_capacity" || f.mode == "workflow_capacity" {
			n := 129
			if f.mode == "workflow_capacity" {
				n = 17
			}
			checks = []map[string]any{}
			for i := 0; i < n; i++ {
				checks = append(checks, map[string]any{"context": fmt.Sprintf("build-%d", i), "app_id": 10})
			}
		}
		body = map[string]any{"required_status_checks": map[string]any{"strict": true, "checks": checks, "contexts": []string{}}}
	case r.URL.Path == "/repos/owner/repo/rules/branches/main":
		body = []any{}
	case r.URL.Path == candidatePullPath:
		pull := candidatePullFixture()
		if f.mode == "pending" {
			pull["mergeable"] = nil
		}
		if f.mode == "conflicting" {
			pull["mergeable"] = false
		}
		if (f.mode == "move_candidate" || f.mode == "empty_moved") && call > 2 {
			pull["head"].(map[string]any)["sha"] = strings.Repeat("d", 40)
		}
		body = pull
	case r.URL.Path == candidateBranchPath:
		body = candidateBranchFixture()
	case r.URL.Path == candidateCommitPath:
		body = candidateCommitFixture(candidateBase, candidateHead)
	case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/commits/"):
		parts := strings.Split(r.URL.Path, "/")
		head, kind := parts[5], parts[6]
		if head != candidateHead && head != candidateMerge {
			f.t.Error("unselected commit", head)
			http.NotFound(w, r)
			return
		}
		switch kind {
		case "check-suites":
			n := 2
			if f.mode == "newer_pending" {
				n = 3
			}
			if f.mode == "workflow_capacity" {
				n = 17
			}
			if head == candidateMerge && (f.mode == "fallback" || f.mode == "move_empty_merge") {
				n = 0
				if f.mode == "move_empty_merge" && call > 2 {
					n = 1
				}
			}
			rows := []any{}
			if n > 0 {
				rows = append(rows, map[string]any{"id": 40, "head_sha": head, "app": map[string]any{"id": 10}})
			}
			body = map[string]any{"total_count": n, "check_suites": rows}
		case "check-runs":
			if r.URL.Query().Get("filter") != "all" {
				f.t.Error("missing full check filter")
			}
			if f.mode == "partial_merge" && head == candidateMerge {
				http.Error(w, "PRIVATE_PROVIDER_BODY", http.StatusForbidden)
				return
			}
			checks := f.checks(head, call)
			body = ciEnvelope(checks, len(checks))
		case "statuses":
			statuses := []map[string]any{}
			if f.mode == "move_status_order" {
				for _, id := range []int64{91, 90} {
					s := ciStatusFixture(id)
					s["context"] = "build"
					if id == 90 {
						s["state"] = "failure"
					}
					statuses = append(statuses, s)
				}
				if call > 1 {
					statuses[0], statuses[1] = statuses[1], statuses[0]
				}
			}
			body = statuses
		default:
			f.t.Error("unrelated commit endpoint", r.URL.Path)
			http.NotFound(w, r)
			return
		}
	case r.URL.Path == actionsRunsPath:
		head := r.URL.Query().Get("head_sha")
		if head != candidateHead && head != candidateMerge {
			f.t.Error("unscoped Actions query", r.URL.String())
		}
		rows := []map[string]any{f.run(50, head), f.run(60, head)}
		if f.mode == "newer_pending" {
			rows = append(rows, f.run(51, head))
		}
		if f.mode == "workflow_capacity" {
			rows = []map[string]any{}
			for i := 0; i < 17; i++ {
				run := f.run(50, head)
				run["id"], run["check_suite_id"], run["workflow_id"] = 50+i, 40+i, 30+i
				rows = append(rows, run)
			}
		}
		if suite := r.URL.Query().Get("check_suite_id"); suite != "" {
			filtered := []map[string]any{}
			for _, row := range rows {
				if fmt.Sprint(row["check_suite_id"]) == suite {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		} else if f.mode == "move_actions" && headWalk > 1 {
			rows[0]["run_attempt"] = 3
		}
		body = actionsEnvelope("workflow_runs", rows, len(rows))
	case strings.HasPrefix(r.URL.Path, actionsRunsPath+"/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, actionsRunsPath+"/"), "/")
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		// Only the relevant latest workflow may fan out to numbered jobs.
		if id == 60 || f.mode == "newer_pending" && id != 51 {
			f.t.Error("fetched irrelevant/superseded workflow", r.URL.Path)
		}
		head := candidateMerge
		if f.mode == "fallback" || f.mode == "move_empty_merge" || f.mode == "conflicting" {
			head = candidateHead
		}
		if len(parts) == 1 {
			body = f.run(id, head)
		} else if len(parts) == 4 && parts[1] == "attempts" && parts[3] == "jobs" {
			job := actionsJobFixture(70)
			job["head_sha"] = head
			if id == 51 {
				job["id"], job["run_id"], job["check_run_url"], job["status"], job["conclusion"], job["started_at"], job["completed_at"] = 72, 51, "https://api.github.com/repos/owner/repo/check-runs/1072", "queued", nil, nil, nil
			}
			body = actionsEnvelope("jobs", []map[string]any{job}, 1)
		} else {
			f.t.Error("unexpected Actions resource", r.URL.Path)
			http.NotFound(w, r)
			return
		}
	default:
		f.t.Error("unrelated endpoint", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func TestObserveRequiredCISelectsMergeOrProvenEmptyHeadFallback(t *testing.T) {
	for _, mode := range []string{"merge", "fallback", "conflicting"} {
		t.Run(mode, func(t *testing.T) {
			f := newCIObservationFixture(t, mode)
			start := time.Now()
			got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			if err != nil || got.State != "satisfied" || got.Assessment.State != got.State || got.Reason != "" || got.StartedAt.Before(start) || got.ObservedAt.Before(got.StartedAt) || got.Results == nil || got.Actions == nil || len(got.Attempts) != 1 || got.Attempts[0].RunID != 50 {
				t.Fatal("not a complete selected observation", got, err)
			}
			want, selection := candidateMerge, "test_merge"
			if mode != "merge" {
				want, selection = candidateHead, "head"
			}
			if got.SelectedCommitSHA != want || got.Selection != selection || got.Results.HeadSHA != want || got.Actions.HeadSHA != want {
				t.Fatal("incorrect CI commit selection", got)
			}
			if mode == "fallback" && (got.TestMergeResults == nil || len(got.TestMergeResults.Checks) != 0 || len(got.TestMergeResults.Statuses) != 0) {
				t.Fatal("head fallback omitted complete merge absence")
			}
			if mode == "conflicting" && got.MergeState != "conflicting" {
				t.Fatal("lost source merge conflict")
			}
			paths, counts := f.snapshot()
			branchReads := counts[candidateBranchPath]
			wantBranchReads := 4
			if mode == "conflicting" {
				wantBranchReads = 0
			}
			if branchReads != wantBranchReads {
				t.Fatal("candidate branch identity reads changed", branchReads)
			}
			// Two verified candidate scans add four independently scoped branch reads.
			if len(paths) > 36 {
				t.Fatal("unexpected provider fanout", len(paths))
			}
			for _, path := range paths {
				if strings.Contains(path, "/60/") || mode == "merge" && strings.Contains(path, "/commits/"+candidateHead+"/") {
					t.Fatal("optional workflow/head fetched", path)
				}
			}
		})
	}
}

func TestObserveRequiredCIEmptyAuthorityRechecksSourceWithoutResultCalls(t *testing.T) {
	for _, mode := range []string{"empty", "empty_moved"} {
		t.Run(mode, func(t *testing.T) {
			f := newCIObservationFixture(t, mode)
			got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			want := "not_required"
			if mode == "empty_moved" {
				want = "unproven"
			}
			if err != nil || got.State != want || got.Results != nil || got.Actions != nil || got.Attempts != nil || got.Assessment.State != "not_required" {
				t.Fatal("empty authority fabricated inventories or skipped recheck", got, err)
			}
			paths, _ := f.snapshot()
			for _, path := range paths {
				if strings.Contains(path, "/actions/") || strings.Contains(path, "/commits/") && !strings.Contains(path, "/git/commits/") {
					t.Fatal("empty requirements performed CI fanout", path)
				}
			}
		})
	}
}

func TestObserveRequiredCIIncompleteAndMovingFactsNeverReturnStaleGreen(t *testing.T) {
	for _, mode := range []string{"partial_merge", "pending", "wrong_app", "unlinked_suite", "newer_pending", "move_authority", "move_candidate", "move_results", "move_actions", "move_status_order", "move_empty_merge"} {
		t.Run(mode, func(t *testing.T) {
			f := newCIObservationFixture(t, mode)
			got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			if got.State == "satisfied" || got.State == "not_required" || mode == "partial_merge" && err == nil || mode != "partial_merge" && err != nil {
				t.Fatal("partial/moving facts gained credit", got, err)
			}
			if strings.HasPrefix(mode, "move_") && got.Assessment.State != "satisfied" {
				t.Fatal("coherence downgrade rewrote retained pure assessment", got.Assessment)
			}
			if mode == "newer_pending" && (got.State != "pending" || len(got.Attempts) != 1 || got.Attempts[0].RunID != 51) {
				t.Fatal("older green masked new workflow run", got, err)
			}
			if (mode == "wrong_app" || mode == "unlinked_suite") && len(got.Attempts) != 0 {
				t.Fatal("unproven context/app/suite origin caused workflow fanout", got.Attempts)
			}
			paths, _ := f.snapshot()
			if mode == "partial_merge" {
				for _, path := range paths {
					if strings.Contains(path, "/commits/"+candidateHead+"/") {
						t.Fatal("partial merge fell back to head", path)
					}
				}
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("unsafe provider error", err)
			}
		})
	}
}

func TestObserveRequiredCIBoundsAndInvalidSelectors(t *testing.T) {
	for _, mode := range []string{"context_capacity", "workflow_capacity"} {
		t.Run(mode, func(t *testing.T) {
			f := newCIObservationFixture(t, mode)
			got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			if err != nil || got.State != "unproven" || !strings.Contains(got.Reason, "capacity") || len(got.Attempts) != 0 {
				t.Fatal("capacity permitted fanout", got, err)
			}
			paths, _ := f.snapshot()
			for _, path := range paths {
				if strings.Contains(path, "/attempts/") || mode == "context_capacity" && strings.Contains(path, "/pulls/") {
					t.Fatal("capacity continued fetching", path)
				}
			}
		})
	}
	f := newCIObservationFixture(t, "merge")
	for _, edit := range []func(*CITarget){func(v *CITarget) { v.Repository = "owner/repo/extra" }, func(v *CITarget) { v.RepositoryID = 0 }, func(v *CITarget) { v.PullNumber = 0 }, func(v *CITarget) { v.HeadSHA = "ABC" }, func(v *CITarget) { v.BaseSHA = "ABC" }, func(v *CITarget) { v.BaseRef = "../main" }} {
		target := ciObservationTarget()
		edit(&target)
		if _, err := f.g.ObserveRequiredCI(t.Context(), target); err == nil {
			t.Fatal("invalid selector accepted")
		}
	}
	paths, _ := f.snapshot()
	if len(paths) != 0 {
		t.Fatal("invalid selector performed I/O", paths)
	}
}

func TestObserveRequiredCISharedCallerDeadlineAndCancellation(t *testing.T) {
	f := newCIObservationFixture(t, "deadline")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := f.g.ObserveRequiredCI(ctx, ciObservationTarget())
	if err == nil || got.State == "satisfied" || time.Since(start) > time.Second {
		t.Fatal("deadline not shared across phases", got, err, time.Since(start))
	}
	paths, _ := f.snapshot()
	if len(paths) < 2 || len(paths) > 3 {
		t.Fatal("fixture did not cross read phases before cancellation", paths)
	}
	f = newCIObservationFixture(t, "merge")
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if got, err = f.g.ObserveRequiredCI(ctx, ciObservationTarget()); err == nil || got.State == "satisfied" {
		t.Fatal("cancelled collector succeeded", got, err)
	}
	paths, _ = f.snapshot()
	if len(paths) != 0 {
		t.Fatal("cancelled collector queried provider", paths)
	}
}
