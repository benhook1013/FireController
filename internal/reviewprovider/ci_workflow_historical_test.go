package reviewprovider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func ciHistoricalMatrixFixture(t *testing.T, mode string) *ciObservationFixture {
	t.Helper()
	baseMode := "skip"
	switch mode {
	case "event":
		baseMode = "different event"
	case "rerun repair":
		baseMode = "repaired"
	case "recheck move":
		baseMode = "new run during recheck"
	case "capacity":
		baseMode = "capacity"
	}
	f := ciSkippedRunFixture(t, baseMode)
	original := f.g.baseURL
	var heads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "head mismatch" && r.URL.Path == candidateBranchPath {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": strings.Repeat("d", 40)}})
			return
		}
		// A verified empty test-merge inventory selects the actual pinned head.
		if strings.Contains(r.URL.Path, "/commits/"+candidateMerge+"/") {
			switch {
			case strings.HasSuffix(r.URL.Path, "/check-suites"):
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_suites": []any{}})
			case strings.HasSuffix(r.URL.Path, "/check-runs"):
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []any{}})
			case strings.HasSuffix(r.URL.Path, "/statuses"):
				_ = json.NewEncoder(w).Encode([]any{})
			}
			return
		}
		if r.URL.Path == actionsRunsPath && r.URL.Query().Get("check_suite_id") == "" && heads.Add(1) == 2 && mode == "recheck fail" {
			http.Error(w, "unavailable", 503)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, original+r.URL.RequestURI(), nil)
		if err != nil {
			t.Error(err)
			return
		}
		req.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var value any
		if json.Unmarshal(data, &value) != nil {
			t.Error("fixture JSON")
			return
		}
		var rewrite func(any)
		rewrite = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for key, child := range x {
					if key == "head_sha" {
						x[key] = candidateHead
					} else {
						rewrite(child)
					}
				}
			case []any:
				for _, child := range x {
					rewrite(child)
				}
			}
		}
		rewrite(value)
		if strings.Contains(r.URL.Path, "/attempts/") && strings.HasSuffix(r.URL.Path, "/jobs") && strings.HasPrefix(r.URL.Path, actionsRunsPath+"/6") || mode == "capacity" && strings.Contains(r.URL.Path, "/attempts/") && r.URL.Path != actionsJobsPath {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, actionsRunsPath+"/"), "/")
			run, _ := strconv.ParseInt(parts[0], 10, 64)
			job := func(id int64, name, conclusion string) map[string]any {
				j := actionsJobFixture(id)
				j["run_id"], j["head_sha"], j["name"], j["conclusion"] = run, candidateHead, name, conclusion
				return j
			}
			jobs := []map[string]any{}
			if run == 60 {
				for n := int64(1); n <= 4; n++ {
					conclusion := "failure"
					if mode == "rerun repair" {
						conclusion = "success"
					}
					jobs = append(jobs, job(600+n, "Build and Test (account-service / "+strconv.FormatInt(n, 10)+")", conclusion))
				}
				gateConclusion := "failure"
				if mode == "rerun repair" {
					gateConclusion = "success"
				}
				jobs = append(jobs, job(605, "Validation Gate", gateConclusion))
				if mode == "duplicate" {
					jobs = []map[string]any{job(601, "Build and Test (account-service / 1)", "failure"), job(602, "Build and Test (account-service / 1)", "failure"), job(605, "Validation Gate", "failure")}
				}
				if mode == "invalid" {
					jobs[0]["head_sha"] = strings.Repeat("f", 40)
				}
			} else {
				jobs = []map[string]any{job(run*10, "Build and Test (${{ matrix.module }} / ${{ matrix.partition }})", "skipped"), job(run*10+1, "Validation Gate", "success")}
				if mode == "newer duplicate" {
					jobs = append(jobs, job(run*10+2, "Build and Test (account-service / 1)", "skipped"), job(run*10+3, "Build and Test (account-service / 1)", "skipped"))
					for n := int64(2); n <= 4; n++ {
						jobs = append(jobs, job(run*10+2+n, "Build and Test (account-service / "+strconv.FormatInt(n, 10)+")", "success"))
					}
				}
				if mode == "unknown jobs" {
					jobs = []map[string]any{}
				}
				if mode == "success" || mode == "pending" || mode == "cancelled" || mode == "failure" {
					for n := int64(1); n <= 4; n++ {
						j := job(run*10+1+n, "Build and Test (account-service / "+strconv.FormatInt(n, 10)+")", mode)
						if mode == "pending" {
							j["status"], j["conclusion"], j["completed_at"] = "in_progress", nil, nil
						}
						jobs = append(jobs, j)
					}
				}
			}
			value = actionsEnvelope("jobs", jobs, len(jobs))
		}
		w.WriteHeader(response.StatusCode)
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	f.g = testGitHub(server, "synthetic-ci-observation-token")
	return f
}

func TestHistoricalMatrixFailureIsLabelledWithoutChangingGateOrAttemptWalk(t *testing.T) {
	old := ciHistoricalMatrixFixture(t, "matrix")
	gate, proven, err := old.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
	if err != nil || len(proven) != 0 {
		t.Fatal("fixture did not preserve exact-name refusal", gate, proven, err)
	}
	f := ciHistoricalMatrixFixture(t, "matrix")
	got, rows, err := f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", []int64{31}, nil, []int64{31})
	if err != nil || len(rows) != 1 || rows[0].Basis != CIWorkflowHistoricalBasis || rows[0].RunID != 60 || rows[0].JobID != 601 || rows[0].Name != "Build and Test (account-service / 1)" || !CIWorkflowFailureApplies(got, rows[0]) || !reflect.DeepEqual(workflowGateFacts(t, gate), workflowGateFacts(t, got)) {
		t.Fatal("historical matrix evidence lost or credited as current CI", got, rows, err)
	}
	_, prior := old.snapshot()
	_, current := f.snapshot()
	for path, count := range prior {
		if strings.Contains(path, "/attempts/") && current[path] != count {
			t.Fatal("historical attention changed attempt walk", path, prior, current)
		}
	}
	for path, count := range current {
		if strings.Contains(path, "/attempts/") && prior[path] != count {
			t.Fatal("historical attention added attempt reads", path, prior, current)
		}
	}
	if current[actionsRunsPath] != prior[actionsRunsPath]+1 {
		t.Fatal("historical attention must add exactly one final inventory recheck", prior, current)
	}
	if len(got.WorkflowAttempts) != len(gate.WorkflowAttempts) || got.WorkflowActions == nil || gate.WorkflowActions != nil || got.State != "pending" {
		t.Fatal("historical attention changed gate or missing final inventory proof", got.State)
	}
	for _, damage := range []string{"missing final check", "wrong attempt", "unread older", "basis"} {
		t.Run(damage, func(t *testing.T) {
			o, row := got, rows[0]
			switch damage {
			case "missing final check":
				o.WorkflowActions = nil
			case "wrong attempt":
				row.RunAttempt++
			case "unread older":
				o.WorkflowAttempts = o.WorkflowAttempts[:1]
			case "basis":
				row.Basis = "current_failure"
			}
			if CIWorkflowFailureApplies(o, row) {
				t.Fatal("unproven historical failure accepted", damage)
			}
		})
	}
}

func TestHistoricalMatrixFailureKeepsRepairAndCollectionGuards(t *testing.T) {
	for _, mode := range []string{"success", "pending", "cancelled", "failure", "rerun repair", "event", "duplicate", "newer duplicate", "invalid", "unknown jobs", "recheck move", "recheck fail", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			f := ciHistoricalMatrixFixture(t, mode)
			got, rows, err := f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", []int64{31}, nil, []int64{31})
			if mode == "failure" {
				if len(rows) != 1 || rows[0].RunID != 61 || rows[0].Basis != "" {
					t.Fatal("actual newer failure was replaced by history", rows, err)
				}
			} else if len(rows) != 0 {
				t.Fatal("superseded/invalid/unread failure gained historical attention", mode, rows, err)
			}
			if got.State != "pending" || got.Selection != "head" || got.SelectedCommitSHA != candidateHead {
				t.Fatal("historical collection changed required gate", mode, got, err)
			}
			if mode == "unknown jobs" || mode == "capacity" {
				_, calls := f.snapshot()
				if calls[actionsRunsPath+"/60/attempts/1/jobs"] != 0 {
					t.Fatal("history-only collection was added", mode, calls)
				}
			}
		})
	}
}

func TestHistoricalMatrixAttentionAlsoPreservesHeadOnlyMismatch(t *testing.T) {
	f := ciHistoricalMatrixFixture(t, "head mismatch")
	got, rows, err := f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", []int64{31}, []int64{31}, []int64{31})
	if err != nil || len(rows) != 1 || rows[0].Basis != CIWorkflowHistoricalBasis || !CIWorkflowFailureApplies(got, rows[0]) || CIWorkflowAttentionBasis(got) != CIWorkflowHeadOnlyBasis || got.State != "unproven" || got.Reason != "ci_merge_base_mismatch" || got.SelectedCommitSHA != "" || len(got.Assessment.Checks) != 0 {
		t.Fatal("historical evidence claimed applicable comparison", got, rows, err)
	}
}
