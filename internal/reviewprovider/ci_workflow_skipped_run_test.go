package reviewprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The required workflow remains pending. A separate selected workflow has a
// real failed execution and a newer run explicitly skipping that same job.
func ciSkippedRunFixture(t *testing.T, mode string) *ciObservationFixture {
	t.Helper()
	f := &ciObservationFixture{t: t, mode: "merge", counts: map[string]int{}}
	run := func(id int64) map[string]any {
		value := f.run(60, candidateMerge)
		value["id"], value["check_suite_id"], value["run_number"], value["run_attempt"] = id, id-19, id-59, int64(1)
		if id == 60 {
			value["conclusion"] = "failure"
			if mode == "repaired" || mode == "rerun-during-read" {
				value["run_attempt"], value["conclusion"] = int64(2), "success"
			}
		} else if mode == "pending" {
			value["status"], value["conclusion"] = "in_progress", nil
		} else if mode == "cancelled" {
			value["conclusion"] = "cancelled"
		} else if mode == "new failure" {
			value["conclusion"] = "failure"
		} else if mode == "different event" {
			value["event"] = "workflow_dispatch"
		}
		return value
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-ci-observation-token" {
			t.Error("attention changed method or credential")
		}
		var body any
		switch {
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			status := ciStatusFixture(90)
			status["context"], status["state"] = "build", "pending"
			body = []any{status}
		case r.URL.Path == actionsRunsPath:
			if suite := r.URL.Query().Get("check_suite_id"); suite == "40" {
				f.serve(w, r)
				return
			} else if suite != "" {
				number, _ := strconv.ParseInt(suite, 10, 64)
				value := run(number + 19)
				if mode == "rerun-during-read" && number == 41 {
					value["run_attempt"], value["conclusion"] = int64(1), "failure"
				}
				body = actionsEnvelope("workflow_runs", []map[string]any{value}, 1)
			} else {
				f.mu.Lock()
				f.headWalks++
				headWalk := f.headWalks
				f.mu.Unlock()
				rows := []map[string]any{f.run(50, candidateMerge), run(60), run(61)}
				if mode == "rerun-during-read" {
					rows[1]["run_attempt"], rows[1]["conclusion"] = int64(1), "failure"
				}
				if mode == "capacity" {
					for id := int64(62); id <= 93; id++ {
						rows = append(rows, run(id))
					}
				}
				if headWalk > 1 && mode == "new run during recheck" {
					rows = append(rows, run(62))
				}
				if headWalk > 1 && mode == "rerun during recheck" {
					rows[1]["run_attempt"] = int64(2)
				}
				body = actionsEnvelope("workflow_runs", rows, len(rows))
			}
		case strings.HasPrefix(r.URL.Path, actionsRunsPath+"/6") || mode == "capacity" && strings.HasPrefix(r.URL.Path, actionsRunsPath+"/") && r.URL.Path != actionsCurrentPath && r.URL.Path != actionsJobsPath:
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, actionsRunsPath+"/"), "/")
			id, _ := strconv.ParseInt(parts[0], 10, 64)
			if len(parts) == 1 {
				body = run(id)
				break
			}
			jobID := id + 30
			job := actionsJobFixture(jobID)
			job["run_id"], job["head_sha"], job["name"] = id, candidateMerge, "Dev Tool Contract Checks"
			job["conclusion"] = "skipped"
			if id == 60 {
				job["conclusion"] = "failure"
				if mode == "repaired" || mode == "rerun-during-read" {
					job["conclusion"] = "success"
				}
			} else {
				switch mode {
				case "success":
					job["conclusion"] = "success"
				case "cancelled":
					job["conclusion"] = "cancelled"
				case "pending":
					job["status"], job["conclusion"], job["completed_at"] = "in_progress", nil, nil
				case "new failure":
					job["conclusion"] = "failure"
				case "missing":
					job["name"], job["conclusion"] = "Metadata only", "success"
				}
			}
			jobs := []map[string]any{job}
			if mode == "matrix ambiguity" && id > 60 {
				duplicate := actionsJobFixture(jobID + 100)
				duplicate["run_id"], duplicate["head_sha"], duplicate["name"], duplicate["conclusion"] = id, candidateMerge, "Dev Tool Contract Checks", "skipped"
				jobs = append(jobs, duplicate)
			}
			body = actionsEnvelope("jobs", jobs, len(jobs))
		case strings.HasSuffix(r.URL.Path, "/check-suites"):
			recorded := httptest.NewRecorder()
			f.serve(recorded, r)
			var envelope map[string]any
			_ = json.Unmarshal(recorded.Body.Bytes(), &envelope)
			envelope["total_count"] = 3
			if mode == "capacity" {
				envelope["total_count"] = 35
			}
			body = envelope
		default:
			f.serve(w, r)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.counts[r.URL.Path]++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	f.g = testGitHub(server, "synthetic-ci-observation-token")
	return f
}

func TestWorkflowAttentionRetainsFailedExecutionAcrossExplicitSkippedWrapper(t *testing.T) {
	base := ciSkippedRunFixture(t, "skip")
	gate, err := base.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	if err != nil || gate.State != "pending" {
		t.Fatal(gate.State, err)
	}
	f := ciSkippedRunFixture(t, "skip")
	got, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
	if err != nil || len(failures) != 1 || failures[0] != (CIWorkflowFailure{WorkflowID: 31, RunID: 60, RunAttempt: 1, JobID: 90, CheckRunID: 1090, Name: "Dev Tool Contract Checks", Conclusion: "failure"}) || !CIWorkflowFailureApplies(got, failures[0]) {
		t.Fatal("explicit skip hid an actual applicable failed execution", failures, err)
	}
	if !reflect.DeepEqual(workflowGateFacts(t, gate), workflowGateFacts(t, got)) {
		t.Fatal("optional attention changed original required gate facts")
	}
	for name, change := range map[string]func(*CIObservation){
		"missing proof":           func(o *CIObservation) { o.WorkflowAttempts = nil },
		"missing recheck":         func(o *CIObservation) { o.WorkflowActions = nil },
		"foreign repository":      func(o *CIObservation) { o.Target.RepositoryID++ },
		"foreign repository name": func(o *CIObservation) { o.Target.Repository = "foreign/repo" },
		"wrong CI commit":         func(o *CIObservation) { o.SelectedCommitSHA = strings.Repeat("a", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			value := got
			change(&value)
			if CIWorkflowFailureApplies(value, failures[0]) {
				t.Fatal("unverified older failure accepted")
			}
		})
	}
	for name, change := range map[string]func(*CIWorkflowFailure){
		"old attempt": func(f *CIWorkflowFailure) { f.RunAttempt++ },
		"wrong job":   func(f *CIWorkflowFailure) { f.JobID++ },
		"wrong check": func(f *CIWorkflowFailure) { f.CheckRunID++ },
	} {
		t.Run(name, func(t *testing.T) {
			value := failures[0]
			change(&value)
			if CIWorkflowFailureApplies(got, value) {
				t.Fatal("fabricated older identity accepted")
			}
		})
	}
}

func TestWorkflowAttentionDoesNotReviveRepairedOrSupersededExecution(t *testing.T) {
	for _, mode := range []string{"repaired", "rerun-during-read", "success", "pending", "cancelled", "missing", "matrix ambiguity", "different event", "new run during recheck", "rerun during recheck", "new failure"} {
		t.Run(mode, func(t *testing.T) {
			f := ciSkippedRunFixture(t, mode)
			got, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
			want := 0
			if mode == "new failure" {
				want = 1
			}
			if err != nil || got.State != "pending" || len(failures) != want {
				t.Fatal(mode, got.State, failures, err)
			}
			if want == 1 && (failures[0].RunID != 61 || !CIWorkflowFailureApplies(got, failures[0])) {
				t.Fatal("new failure did not supersede prior execution", failures)
			}
			_, counts := f.snapshot()
			if mode == "success" || mode == "pending" || mode == "cancelled" || mode == "missing" || mode == "matrix ambiguity" || mode == "new failure" {
				if counts[actionsRunsPath+"/60"] != 0 {
					t.Fatal("unnecessary older-run fanout after proof settled", counts)
				}
			}
		})
	}
}

func TestWorkflowAttentionSkippedHistoryHasFiniteAttemptFanout(t *testing.T) {
	f := ciSkippedRunFixture(t, "capacity")
	gate, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
	var warning *CIWorkflowAttentionError
	if !errors.As(err, &warning) || gate.State != "pending" || len(failures) != 0 || len(gate.Attempts)+len(gate.WorkflowAttempts) != 32 {
		t.Fatal("unbounded or miscredited history walk", gate.State, len(gate.WorkflowAttempts), failures, err)
	}
	_, counts := f.snapshot()
	if counts[fmt.Sprintf("%s/60", actionsRunsPath)] != 0 {
		t.Fatal("walk exceeded hard total attempt bound", counts)
	}
}
