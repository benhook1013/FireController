package reviewprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type distinctCIMember struct {
	head, merge                            string
	pull, run, workflow, suite, check, job int64
}

func newDistinctCIFixture(t *testing.T, mode string) (*GitHub, []CITarget, *atomic.Int64) {
	t.Helper()
	members := [2]distinctCIMember{
		{strings.Repeat("a", 40), strings.Repeat("c", 40), 7, 50, 30, 40, 1070, 70},
		{strings.Repeat("d", 40), strings.Repeat("e", 40), 8, 150, 130, 140, 1170, 170},
	}
	calls := &atomic.Int64{}
	run := func(m distinctCIMember) map[string]any {
		r := actionsRunFixture()
		r["id"], r["head_sha"], r["check_suite_id"], r["workflow_id"] = m.run, m.merge, m.suite, m.workflow
		return r
	}
	suiteNode := func(m distinctCIMember) map[string]any {
		n := suiteBatchNode(m.merge, 1)
		n["checkSuites"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["databaseId"] = m.suite
		return n
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-distinct-ci" {
			t.Error("wrong credential binding")
		}
		var body any
		if r.URL.Path == "/graphql" {
			if r.Method != "POST" {
				t.Error("wrong graphql method")
			}
			var req struct{ Variables map[string]string }
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				t.Error("query decode")
			}
			if req.Variables["h0"] != members[0].merge || req.Variables["h1"] != members[1].merge {
				t.Error("mixed commit selectors", req)
			}
			a, b := suiteNode(members[0]), suiteNode(members[1])
			if mode == "swapped" {
				a, b = b, a
			}
			if mode == "foreign" {
				a["oid"] = strings.Repeat("f", 40)
			}
			json.NewEncoder(w).Encode(suiteBatchEnvelope(a, b))
			return
		}
		if r.Method != "GET" {
			t.Error("unexpected mutation")
		}
		switch r.URL.Path {
		case "/repos/owner/repo/branches/main/protection":
			body = map[string]any{"required_status_checks": map[string]any{"strict": true, "checks": []any{map[string]any{"context": "build", "app_id": 10}}, "contexts": []string{}}}
		case "/repos/owner/repo/rules/branches/main":
			body = []any{}
		case candidateBranchPath:
			body = candidateBranchFixture()
		default:
			found := false
			for _, m := range members {
				switch {
				case r.URL.Path == fmt.Sprintf("/repos/owner/repo/pulls/%d", m.pull):
					p := candidatePullFixture()
					p["number"], p["merge_commit_sha"] = m.pull, m.merge
					p["head"].(map[string]any)["sha"] = m.head
					body = p
				case r.URL.Path == "/repos/owner/repo/git/commits/"+m.merge:
					c := candidateCommitFixture(candidateBase, m.head)
					c["sha"] = m.merge
					body = c
				case r.URL.Path == "/repos/owner/repo/commits/"+m.merge+"/check-suites":
					body = map[string]any{"total_count": 1, "check_suites": []any{map[string]any{"id": m.suite, "head_sha": m.merge, "app": map[string]any{"id": 10}}}}
				case r.URL.Path == "/repos/owner/repo/commits/"+m.merge+"/check-runs":
					if r.URL.Query().Get("filter") != "all" {
						t.Error("lost historical check filter")
					}
					c := ciCheckFixture(m.check)
					c["name"], c["head_sha"], c["check_suite"] = "build", m.merge, map[string]any{"id": m.suite}
					body = ciEnvelope([]map[string]any{c}, 1)
				case r.URL.Path == "/repos/owner/repo/commits/"+m.merge+"/statuses":
					body = []any{}
				case r.URL.Path == actionsRunsPath && r.URL.Query().Get("head_sha") == m.merge:
					if s := r.URL.Query().Get("check_suite_id"); s != "" && s != strconv.FormatInt(m.suite, 10) {
						t.Error("foreign suite")
					}
					body = actionsEnvelope("workflow_runs", []map[string]any{run(m)}, 1)
				case r.URL.Path == fmt.Sprintf("%s/%d", actionsRunsPath, m.run):
					body = run(m)
				case r.URL.Path == fmt.Sprintf("%s/%d/attempts/2/jobs", actionsRunsPath, m.run):
					j := actionsJobFixture(m.job)
					j["run_id"], j["head_sha"], j["check_run_url"] = m.run, m.merge, fmt.Sprintf("https://api.github.com/repos/owner/repo/check-runs/%d", m.check)
					body = actionsEnvelope("jobs", []map[string]any{j}, 1)
				default:
					continue
				}
				found = true
				break
			}
			if !found {
				t.Error("unexpected endpoint/selector", r.URL.Path, r.URL.Query())
				http.NotFound(w, r)
				return
			}
		}
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	targets := []CITarget{}
	for _, m := range members {
		targets = append(targets, CITarget{Repository: "owner/repo", RepositoryID: 20, PullNumber: m.pull, HeadSHA: m.head, BaseSHA: candidateBase, BaseRef: "main"})
	}
	return testGitHub(server, "synthetic-distinct-ci"), targets, calls
}

func TestCIFullCountDistinctMembersEquivalent(t *testing.T) {
	ordinary, targets, ordinaryCalls := newDistinctCIFixture(t, "")
	want := make([]CIObservation, 2)
	for i, target := range targets {
		var err error
		want[i], err = ordinary.ObserveCIGate(t.Context(), target, "main")
		if err != nil || want[i].State != "satisfied" {
			t.Fatal(want[i], err)
		}
	}
	sequential, targets, seqCalls := newDistinctCIFixture(t, "")
	cohort, err := sequential.NewCIGateCohort(t.Context(), targets, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer cohort.Close()
	for i, target := range targets {
		o, e := cohort.ObserveCIGate(t.Context(), target, "main")
		if e != nil || !reflect.DeepEqual(normalizeCohortObservation(o), normalizeCohortObservation(want[i])) {
			t.Fatal(o, e)
		}
	}
	g, targets, calls := newDistinctCIFixture(t, "")
	got, err := g.observeCIGateBatchedCohort(t.Context(), targets, "main")
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range got {
		if row.Err != nil || !reflect.DeepEqual(normalizeCohortObservation(row.Observation), normalizeCohortObservation(want[i])) {
			t.Fatalf("member%d differs: %+v vs %+v", i, row, want[i])
		}
	}
	if calls.Load() != 40 || seqCalls.Load() != 48 || ordinaryCalls.Load() != 50 {
		t.Fatal("actual HTTP counts", calls.Load(), seqCalls.Load(), ordinaryCalls.Load())
	}
	if got[0].Observation.Results.Checks[0].ID == got[1].Observation.Results.Checks[0].ID || got[0].Observation.Attempts[0].RunID == got[1].Observation.Attempts[0].RunID {
		t.Fatal("fixture identities not distinct")
	}
}
func TestCIFullCountDistinctAliasRefusal(t *testing.T) {
	for _, mode := range []string{"swapped", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			g, targets, _ := newDistinctCIFixture(t, mode)
			got, err := g.observeCIGateBatchedCohort(t.Context(), targets, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Err == nil || got[0].Observation.State == "satisfied" || got[0].Observation.Results != nil {
				t.Fatal("foreign member credited", got)
			}
			if mode == "swapped" {
				if got[1].Err == nil || got[1].Observation.State == "satisfied" {
					t.Fatal("swapped alias credited", got)
				}
			} else if got[1].Err != nil || got[1].Observation.State != "satisfied" {
				t.Fatal("healthy distinct member lost", got)
			}
		})
	}
}
