package reviewprovider

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestAuditDistinctCIFullRequestBreakdown(t *testing.T) {
	for _, mode := range []string{"individual", "ordinary_cohort", "candidate_phase_cohort", "batched_cohort"} {
		counts := map[string]int{}
		var mu sync.Mutex
		g, targets, total := newDistinctCIFixture(t, "", func(r *http.Request) {
			p := r.URL.Path
			key := "other"
			switch {
			case p == "/graphql":
				key = "paired_suite_count"
			case strings.HasSuffix(p, "/protection"):
				key = "branch_protection"
			case strings.Contains(p, "/rules/branches/"):
				key = "branch_rules"
			case strings.Contains(p, "/branches/"):
				key = "branch_tip"
			case strings.Contains(p, "/git/commits/"):
				key = "immutable_merge_object"
			case strings.Contains(p, "/pulls/"):
				key = "pull_candidate"
			case strings.HasSuffix(p, "/check-suites"):
				key = "suite_count"
			case strings.HasSuffix(p, "/check-runs"):
				key = "check_runs_all"
			case strings.HasSuffix(p, "/statuses"):
				key = "status_history"
			case strings.HasSuffix(p, "/jobs"):
				key = "exact_attempt_jobs"
			case strings.HasSuffix(p, "/actions/runs"):
				key = "actions_head_inventory"
			case strings.Contains(p, "/actions/runs/"):
				key = "direct_current_run"
			}
			mu.Lock()
			counts[key]++
			mu.Unlock()
		})
		if len(targets) != 2 {
			t.Fatal("wrong fixture membership", len(targets))
		}
		var rows []ciObservationCohortResult
		var err error
		if mode == "individual" {
			for _, target := range targets {
				o, e := g.ObserveCIGate(t.Context(), target, "main")
				if e != nil || o.State != "satisfied" {
					t.Fatal(o, e)
				}
			}
		} else if mode == "ordinary_cohort" {
			c, e := g.NewCIGateCohort(t.Context(), targets, "main")
			if e != nil {
				t.Fatal(e)
			}
			for _, target := range targets {
				o, e := c.ObserveCIGate(t.Context(), target, "main")
				if e != nil || o.State != "satisfied" {
					t.Fatal(o, e)
				}
			}
			c.Close()
		} else if mode == "batched_cohort" {
			rows, err = g.observeCIGateBatchedCohort(t.Context(), targets, "main")
		} else {
			rows, err = g.observeCIGateSourceCohort(t.Context(), targets, "main")
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode == "candidate_phase_cohort" || mode == "batched_cohort" {
			if len(rows) != 2 {
				t.Fatal("missing result members", len(rows))
			}
		}
		for _, r := range rows {
			if r.Err != nil || r.Observation.State != "satisfied" {
				t.Fatal(r)
			}
		}
		sum := 0
		for _, n := range counts {
			sum += n
		}
		if int64(sum) != total.Load() {
			t.Fatal("incomplete accounting", sum, total.Load())
		}
		expected := map[string]int64{"individual": 50, "ordinary_cohort": 48, "candidate_phase_cohort": 44, "batched_cohort": 40}
		if total.Load() != expected[mode] {
			t.Fatal("request count changed", mode, total.Load())
		}
		var keys []string
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Logf("mode=%s distinct_members=2 total=%d", mode, total.Load())
		for _, k := range keys {
			t.Logf("%s=%d", k, counts[k])
		}
	}
}
