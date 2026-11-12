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
	capacity := mode == "capacity" || mode == "capacity latest"
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
		if id == 70 && strings.HasPrefix(mode, "mixed ") || id == 100 && mode == "capacity latest" {
			value["workflow_id"], value["run_attempt"], value["conclusion"] = int64(32), int64(2), "failure"
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
				if strings.HasPrefix(mode, "mixed ") {
					direct := run(70)
					if headWalk > 1 && mode == "mixed repaired" {
						direct["run_attempt"], direct["conclusion"] = int64(3), "success"
					}
					rows = append(rows, direct)
				}
				if mode == "rerun-during-read" {
					rows[1]["run_attempt"], rows[1]["conclusion"] = int64(1), "failure"
				}
				if capacity {
					for id := int64(62); id <= 93; id++ {
						rows = append(rows, run(id))
					}
				}
				if mode == "capacity latest" {
					rows = append(rows, run(100))
				}
				if headWalk > 1 && mode == "new run during recheck" {
					rows = append(rows, run(62))
				}
				if headWalk > 1 && mode == "rerun during recheck" {
					rows[1]["run_attempt"] = int64(2)
				}
				if headWalk > 1 {
					switch mode {
					case "incomplete recheck":
						body = actionsEnvelope("workflow_runs", rows, 1000)
					case "rate-limited recheck":
						w.Header().Set("Retry-After", "42")
						http.Error(w, "PRIVATE_RECHECK_ERROR", http.StatusTooManyRequests)
						return
					case "unavailable recheck", "mixed unavailable recheck":
						http.Error(w, "PRIVATE_RECHECK_ERROR", http.StatusServiceUnavailable)
						return
					}
				}
				if body == nil {
					body = actionsEnvelope("workflow_runs", rows, len(rows))
				}
			}
		case strings.HasPrefix(r.URL.Path, actionsRunsPath+"/6") || strings.HasPrefix(mode, "mixed ") && strings.HasPrefix(r.URL.Path, actionsRunsPath+"/70") || capacity && strings.HasPrefix(r.URL.Path, actionsRunsPath+"/") && r.URL.Path != actionsCurrentPath && r.URL.Path != actionsJobsPath:
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
			if id == 70 && strings.HasPrefix(mode, "mixed ") || id == 100 && mode == "capacity latest" {
				job["conclusion"], job["run_attempt"] = "failure", int64(2)
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
			if strings.HasPrefix(mode, "mixed ") {
				envelope["total_count"] = 4
			}
			if capacity {
				envelope["total_count"] = 35
			}
			if mode == "capacity latest" {
				envelope["total_count"] = 36
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
	base := ciSkippedRunFixture(t, "capacity")
	ordinary, err := base.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	if err != nil || ordinary.State != "pending" {
		t.Fatal(ordinary.State, err)
	}
	f := ciSkippedRunFixture(t, "capacity")
	gate, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
	var warning *CIWorkflowAttentionError
	if !errors.As(err, &warning) || gate.State != "pending" || len(failures) != 0 || len(gate.Attempts)+len(gate.WorkflowAttempts) != 32 {
		t.Fatal("unbounded or miscredited history walk", gate.State, len(gate.WorkflowAttempts), failures, err)
	}
	var provider *Error
	if errors.As(err, &provider) || !errors.Is(err, ErrCIWorkflowAttemptLimit) || !strings.Contains(warning.Cause.Error(), "incomplete") {
		t.Fatalf("local attempt cap lost its distinct incomplete-inventory warning: %v / %+v", err, provider)
	}
	if !reflect.DeepEqual(workflowGateFacts(t, ordinary), workflowGateFacts(t, gate)) {
		t.Fatal("local attempt cap changed required gate facts")
	}
	_, counts := f.snapshot()
	if counts[fmt.Sprintf("%s/60", actionsRunsPath)] != 0 {
		t.Fatal("walk exceeded hard total attempt bound", counts)
	}
}

func TestWorkflowAttentionRecheckProviderFailuresRemainProviderErrors(t *testing.T) {
	base := ciSkippedRunFixture(t, "skip")
	ordinary, err := base.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	if err != nil || ordinary.State != "pending" {
		t.Fatal(ordinary.State, err)
	}
	for _, test := range []struct {
		mode  string
		code  string
		retry int64
	}{
		{"incomplete recheck", "provider_inventory_incomplete", 0},
		{"rate-limited recheck", "provider_rate_limited", 42},
		{"unavailable recheck", "provider_unavailable", 0},
	} {
		t.Run(test.mode, func(t *testing.T) {
			f := ciSkippedRunFixture(t, test.mode)
			gate, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
			var warning *CIWorkflowAttentionError
			var provider *Error
			if !errors.As(err, &warning) || !errors.As(err, &provider) || provider.Code != test.code || provider.RetryAfterSeconds != test.retry || errors.Is(err, ErrCIWorkflowAttemptLimit) || strings.Contains(err.Error(), "PRIVATE_RECHECK_ERROR") {
				t.Fatal("genuine provider failure became a local cap or lost its safe warning", err, provider)
			}
			if len(failures) != 0 || gate.WorkflowActions != nil || !reflect.DeepEqual(workflowGateFacts(t, ordinary), workflowGateFacts(t, gate)) {
				t.Fatal("failed recheck credited old attention or changed required gate facts", failures, gate.State)
			}
		})
	}
}

func TestWorkflowAttentionFinalInventoryRechecksDirectAndSkippedFailures(t *testing.T) {
	for _, mode := range []string{"mixed stable", "mixed repaired", "mixed unavailable recheck"} {
		t.Run(mode, func(t *testing.T) {
			base := ciSkippedRunFixture(t, mode)
			ordinary, err := base.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
			if err != nil {
				t.Fatal(err)
			}
			f := ciSkippedRunFixture(t, mode)
			got, rows, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31, 32})
			if mode == "mixed unavailable recheck" {
				var provider *Error
				if !errors.As(err, &provider) || provider.Code != "provider_unavailable" || got.WorkflowActions != nil {
					t.Fatal("recheck error lost", got, err)
				}
			} else if err != nil || got.WorkflowActions == nil {
				t.Fatal("successful final inventory missing", got, err)
			}
			want := map[int64]bool{31: true, 32: true}
			if mode == "mixed repaired" {
				delete(want, 32)
			}
			if mode == "mixed unavailable recheck" {
				delete(want, 31)
			}
			if got.State != "pending" || len(rows) != len(want) || !reflect.DeepEqual(workflowGateFacts(t, ordinary), workflowGateFacts(t, got)) {
				t.Fatal("recheck changed gate or retained stale failure", got.State, rows, err)
			}
			for _, row := range rows {
				if !want[row.WorkflowID] || !CIWorkflowFailureApplies(got, row) {
					t.Fatal("unexpected or unproven retained row", row)
				}
				delete(want, row.WorkflowID)
			}
			_, calls := f.snapshot()
			if calls[actionsRunsPath+"/70/attempts/2/jobs"] != 1 {
				t.Fatal("direct current numbered attempt was not collected", calls)
			}
		})
	}
}

func TestWorkflowAttentionLatestFailureSurvivesEarlierHistoryBudget(t *testing.T) {
	f := ciSkippedRunFixture(t, "capacity latest")
	got, rows, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31, 32})
	var warning *CIWorkflowAttentionError
	if !errors.As(err, &warning) || !errors.Is(err, ErrCIWorkflowAttemptLimit) || got.State != "pending" || len(rows) != 1 || rows[0].WorkflowID != 32 || rows[0].RunID != 100 || !CIWorkflowFailureApplies(got, rows[0]) {
		t.Fatal("older history starved selected current failure", got.State, rows, err)
	}
	if len(got.Attempts)+len(got.WorkflowAttempts) != 32 {
		t.Fatal("shared attempt budget changed", len(got.Attempts), len(got.WorkflowAttempts))
	}
	if len(got.WorkflowAttempts) < 2 || got.WorkflowAttempts[1].RunID != 100 {
		t.Fatal("latest selected runs were not read before older history")
	}
}
