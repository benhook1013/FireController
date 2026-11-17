//go:build cohortintegration

package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const engineRequiredCandidateHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const engineRequiredCandidateBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const engineRequiredCandidateMerge = "cccccccccccccccccccccccccccccccccccccccc"
const engineRequiredCiHead = engineRequiredCandidateHead
const engineRequiredActionsHead = engineRequiredCandidateHead
const engineRequiredActionsRunsPath = "/repos/owner/repo/actions/runs"
const engineRequiredCandidateBranchPath = "/repos/owner/repo/branches/main"

func engineRequiredActionsRunFixture() map[string]any {
	return map[string]any{"id": 50, "head_sha": engineRequiredActionsHead, "check_suite_id": 40, "workflow_id": 30, "run_number": 7, "run_attempt": 2, "status": "completed", "conclusion": "success", "event": "pull_request", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z", "repository": map[string]any{"id": 801, "full_name": "owner/repo"}, "head_repository": map[string]any{"id": 999, "full_name": "fork/repo"}}
}
func engineRequiredActionsJobFixture(id int64) map[string]any {
	return map[string]any{"id": id, "run_id": 50, "head_sha": engineRequiredActionsHead, "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/" + strconv.FormatInt(id+1000, 10), "name": "test [linux]", "status": "completed", "conclusion": "success", "started_at": "2026-01-01T00:00:01.000Z", "completed_at": "2026-01-01T00:00:02+00:00", "logs_url": "PRIVATE_LOG_URL", "runner_name": "PRIVATE_RUNNER"}
}
func engineRequiredActionsEnvelope(key string, rows []map[string]any, total int) map[string]any {
	return map[string]any{"total_count": total, key: rows}
}
func engineRequiredCandidatePullFixture() map[string]any {
	return map[string]any{
		"title": "PRIVATE_TITLE", "number": 7, "state": "open", "merged": false,
		"head":          map[string]any{"sha": engineRequiredCandidateHead, "repo": map[string]any{"id": 99, "full_name": "fork/repo", "default_branch": "feature"}},
		"base":          map[string]any{"sha": engineRequiredCandidateBase, "ref": "main", "repo": map[string]any{"id": 801, "full_name": "owner/repo", "default_branch": "main"}},
		"changed_files": 2, "mergeable": true, "merge_commit_sha": engineRequiredCandidateMerge,
	}
}
func engineRequiredCandidateCommitFixture(parents ...string) map[string]any {
	rows := make([]map[string]any, 0, len(parents))
	for _, sha := range parents {
		rows = append(rows, map[string]any{"sha": sha})
	}
	return map[string]any{"sha": engineRequiredCandidateMerge, "parents": rows, "message": "PRIVATE_COMMIT_MESSAGE"}
}
func engineRequiredCandidateBranchFixture() map[string]any {
	return map[string]any{"name": "main", "commit": map[string]any{"sha": engineRequiredCandidateBase}}
}
func engineRequiredCiCheckFixture(id int64) map[string]any {
	return map[string]any{"id": id, "name": " build [linux] ", "head_sha": engineRequiredCiHead, "status": "completed", "conclusion": "success", "started_at": "2026-01-01T00:00:01+00:00", "completed_at": "2026-01-01T00:00:02.123Z", "app": map[string]any{"id": 10, "slug": "not-an-identity"}, "check_suite": map[string]any{"id": 20}, "output": map[string]any{"text": "PRIVATE_OUTPUT"}}
}
func engineRequiredCiEnvelope(rows []map[string]any, total int) map[string]any {
	return map[string]any{"total_count": total, "check_runs": rows}
}
func engineRequiredSuiteBatchNode(head string, total int) map[string]any {
	nodes := []any{}
	if total > 0 {
		nodes = append(nodes, map[string]any{"databaseId": int64(119178679699), "commit": map[string]any{"oid": head}, "app": map[string]any{"databaseId": int64(347564)}})
	}
	return map[string]any{"__typename": "Commit", "oid": head, "checkSuites": map[string]any{"totalCount": total, "pageInfo": map[string]any{"hasNextPage": total > 1}, "nodes": nodes}}
}
func engineRequiredSuiteBatchEnvelope(a, b any) map[string]any {
	return map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "owner/repo", "c0": a, "c1": b}}}
}

type engineRequiredDistinctCIMember struct {
	head, merge                            string
	pull, run, workflow, suite, check, job int64
}

func engineRequiredNewDistinctCIFixture(t *testing.T, mode string, duringRead func()) (*reviewprovider.GitHub, []reviewprovider.CITarget, *atomic.Int64) {
	t.Helper()
	members := [2]engineRequiredDistinctCIMember{
		{strings.Repeat("a", 40), strings.Repeat("c", 40), 2, 50, 30, 40, 1070, 70},
		{strings.Repeat("d", 40), strings.Repeat("e", 40), 4, 150, 130, 140, 1170, 170},
	}
	calls := &atomic.Int64{}
	run := func(m engineRequiredDistinctCIMember) map[string]any {
		r := engineRequiredActionsRunFixture()
		r["id"], r["head_sha"], r["check_suite_id"], r["workflow_id"] = m.run, m.merge, m.suite, m.workflow
		return r
	}
	suiteNode := func(m engineRequiredDistinctCIMember) map[string]any {
		n := engineRequiredSuiteBatchNode(m.merge, 1)
		n["checkSuites"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["databaseId"] = m.suite
		return n
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		duringRead()
		if r.Header.Get("Authorization") != "Bearer synthetic-cohort-integration" {
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
			json.NewEncoder(w).Encode(engineRequiredSuiteBatchEnvelope(a, b))
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
		case engineRequiredCandidateBranchPath:
			body = engineRequiredCandidateBranchFixture()
		default:
			found := false
			for _, m := range members {
				switch {
				case r.URL.Path == fmt.Sprintf("/repos/owner/repo/pulls/%d", m.pull):
					p := engineRequiredCandidatePullFixture()
					p["number"], p["merge_commit_sha"] = m.pull, m.merge
					p["head"].(map[string]any)["sha"] = m.head
					body = p
				case r.URL.Path == "/repos/owner/repo/git/commits/"+m.merge:
					c := engineRequiredCandidateCommitFixture(engineRequiredCandidateBase, m.head)
					c["sha"] = m.merge
					body = c
				case r.URL.Path == "/repos/owner/repo/commits/"+m.merge+"/check-suites":
					body = map[string]any{"total_count": 1, "check_suites": []any{map[string]any{"id": m.suite, "head_sha": m.merge, "app": map[string]any{"id": 10}}}}
				case r.URL.Path == "/repos/owner/repo/commits/"+m.merge+"/check-runs":
					if r.URL.Query().Get("filter") != "all" {
						t.Error("lost historical check filter")
					}
					c := engineRequiredCiCheckFixture(m.check)
					c["name"], c["head_sha"], c["check_suite"] = "build", m.merge, map[string]any{"id": m.suite}
					body = engineRequiredCiEnvelope([]map[string]any{c}, 1)
				case r.URL.Path == "/repos/owner/repo/commits/"+m.merge+"/statuses":
					body = []any{}
				case r.URL.Path == engineRequiredActionsRunsPath && r.URL.Query().Get("head_sha") == m.merge:
					if s := r.URL.Query().Get("check_suite_id"); s != "" && s != strconv.FormatInt(m.suite, 10) {
						t.Error("foreign suite")
					}
					body = engineRequiredActionsEnvelope("workflow_runs", []map[string]any{run(m)}, 1)
				case r.URL.Path == fmt.Sprintf("%s/%d", engineRequiredActionsRunsPath, m.run):
					body = run(m)
				case r.URL.Path == fmt.Sprintf("%s/%d/attempts/2/jobs", engineRequiredActionsRunsPath, m.run):
					j := engineRequiredActionsJobFixture(m.job)
					j["run_id"], j["head_sha"], j["check_run_url"] = m.run, m.merge, fmt.Sprintf("https://api.github.com/repos/owner/repo/check-runs/%d", m.check)
					body = engineRequiredActionsEnvelope("jobs", []map[string]any{j}, 1)
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
	targets := []reviewprovider.CITarget{}
	for _, m := range members {
		targets = append(targets, reviewprovider.CITarget{Repository: "owner/repo", RepositoryID: 801, PullNumber: m.pull, HeadSHA: m.head, BaseSHA: engineRequiredCandidateBase, BaseRef: "main"})
	}
	g, err := reviewprovider.NewCohortIntegrationFixture(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return g, targets, calls
}

type requiredEngineReader struct {
	*sourceCITestReader
	github    *reviewprovider.GitHub
	gateReady bool
}

func (r *requiredEngineReader) NewCIGateCohort(ctx context.Context, targets []reviewprovider.CITarget, gate string) (*reviewprovider.CIGateCohort, error) {
	return r.github.NewCIGateCohort(ctx, targets, gate)
}
func (r *requiredEngineReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	if !r.gateReady {
		return r.sourceCITestReader.ObserveRequiredCI(ctx, target)
	}
	return r.github.ObserveCIGate(ctx, target, gate)
}

func TestLifecycleRequiredCIBatchActualPublication(t *testing.T) {
	for _, tc := range []struct{ enabled, watching bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		enabled, watching := tc.enabled, tc.watching
		t.Run(fmt.Sprintf("batch%t_watch%t", enabled, watching), func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			g, targets, calls := engineRequiredNewDistinctCIFixture(t, "", func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				if !s.lanes[lifecycleEnrichment].projects[attemptProject] || len(s.lanes[lifecycleEnrichment].accounts) != 1 {
					t.Error("account/project reservation missing during provider I/O")
				}
			})
			var sourceCalls atomic.Int64
			r := &requiredEngineReader{github: g, sourceCITestReader: &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				sourceCalls.Add(1)
				p := lifecyclePull(n)
				if n == 4 {
					p.HeadSHA = strings.Repeat("d", 40)
				}
				return p, nil
			}}}}
			r.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return sourceCITestObservation(target, "failed"), nil
			}
			lifecycleTick(t, f, s, r)
			lifecycleTick(t, f, s, r)
			for _, target := range targets {
				var before reviews.PullResponse
				projectionRead(t, f, fmt.Sprintf("/pulls/%d", target.PullNumber), &before)
				if before.Pull.CIState == "success" {
					t.Fatal("seed must not already be successful")
				}
			}
			r.gateReady = true
			clock.Add(6 * time.Minute)
			s.options.BatchCIWitnesses = enabled
			calls.Store(0)
			sourceCalls.Store(0)
			first, err := f.e.claimLifecycleLane(t.Context(), map[string]TargetReader{"test-capability": r}, s, lifecycleEnrichment)
			if err != nil || first == nil {
				t.Fatal(first, err)
			}
			var watched <-chan ciWatchReply
			if watching {
				s.readers = map[string]TargetReader{"test-capability": r}
				attachLifecycleHints(t, f, s)
				var cancelWatch context.CancelFunc
				watched, cancelWatch = startCIWatch(t, f, operationReadScope(t, f), []CIWatchTarget{{PullNumber: first.number, HeadSHA: first.comparison.head, BaseSHA: first.comparison.baseSHA}})
				defer cancelWatch()

				ciWatchRegistered(t, s, 1)
			}
			if err = f.e.observeLifecycleCohort(t.Context(), *first, map[string]TargetReader{"test-capability": r}, s); err != nil {
				t.Fatal(err)
			}
			want := int64(48)
			if enabled && !watching {
				want = 40
			}
			if watching {
				reply := receiveCIWatch(t, watched)
				if reply.err != nil || reply.value.State != "passed" {
					t.Fatal("existing watch lost fresh sharing", reply)
				}
			}
			if calls.Load() != want || sourceCalls.Load() != 2 {
				t.Fatal("collector/preparation requests", calls.Load(), sourceCalls.Load(), want)
			}
			for _, target := range targets {
				var persisted reviews.PullResponse
				projectionRead(t, f, fmt.Sprintf("/pulls/%d", target.PullNumber), &persisted)
				if persisted.Pull.CIState != "success" {
					t.Fatal("persisted required CI did not publish", target, persisted.Pull.CIState)
				}
				d := s.due[lifecycleKey{attemptProject, target.PullNumber}]
				if d.ciGateState != "success" || d.ciError != "" || d.ciHead != target.HeadSHA || !d.ci.After(clock.Now()) {
					t.Fatal("required CI not published", target, d.ciGateState, d.ciError, d.ciHead)
				}
			}
			if len(s.lanes[lifecycleEnrichment].accounts) != 0 || len(s.lanes[lifecycleEnrichment].projects) != 0 {
				t.Fatal("reservation leak")
			}
		})
	}
}
