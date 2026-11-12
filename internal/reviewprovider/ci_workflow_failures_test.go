package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The normal CI fixture owns the required gate. Only the explicitly optional
// second workflow gets an additional numbered-attempt response here.
func ciWorkflowFailureFixture(t *testing.T, mode string) *ciObservationFixture {
	t.Helper()
	return ciWorkflowFailureFixtureWithOptionalStart(t, mode, nil)
}

func ciWorkflowFailureFixtureWithOptionalStart(t *testing.T, mode string, optionalStarted func()) *ciObservationFixture {
	t.Helper()
	f := &ciObservationFixture{t: t, mode: "merge", counts: map[string]int{}}
	if mode == "empty" {
		f.mode = "empty"
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, actionsRunsPath+"/60") && !(r.URL.Path == actionsRunsPath && r.URL.Query().Get("check_suite_id") == "41") {
			missingAuthority := mode == "failed and missing" && r.URL.Path == "/repos/owner/repo/branches/main/protection"
			if mode == "passed" || mode == "empty" || !missingAuthority && r.URL.Path != actionsRunsPath && r.URL.Path != actionsCurrentPath && r.URL.Path != actionsJobsPath && !strings.HasSuffix(r.URL.Path, "/check-runs") {
				f.serve(w, r)
				return
			}
			recorded := httptest.NewRecorder()
			f.serve(recorded, r)
			var body map[string]any
			if json.Unmarshal(recorded.Body.Bytes(), &body) != nil {
				t.Error("invalid base fixture response")
				return
			}
			state, conclusion := "in_progress", any(nil)
			if mode == "failed gate" || mode == "failed and missing" {
				state, conclusion = "completed", "failure"
			}
			if missingAuthority {
				requirements := body["required_status_checks"].(map[string]any)
				requirements["checks"] = append(requirements["checks"].([]any), map[string]any{"context": "missing", "app_id": 10})
			} else if r.URL.Path == actionsRunsPath {
				if mode == "rate limit" && r.URL.Query().Get("check_suite_id") == "" {
					run := f.run(60, candidateMerge)
					run["id"], run["check_suite_id"], run["workflow_id"] = int64(61), int64(42), int64(32)
					body["workflow_runs"] = append(body["workflow_runs"].([]any), run)
					body["total_count"] = 3
				}
				for _, value := range body["workflow_runs"].([]any) {
					run := value.(map[string]any)
					if run["workflow_id"] == float64(30) {
						run["status"], run["conclusion"] = state, conclusion
					}
				}
			} else if r.URL.Path == actionsCurrentPath {
				body["status"], body["conclusion"] = state, conclusion
			} else if strings.HasSuffix(r.URL.Path, "/check-runs") || r.URL.Path == actionsJobsPath {
				key := "check_runs"
				if r.URL.Path == actionsJobsPath {
					key = "jobs"
				}
				rows := body[key].([]any)
				first := rows[0].(map[string]any)
				first["status"], first["conclusion"] = state, conclusion
				if state != "completed" {
					first["completed_at"] = nil
				}
				if key == "jobs" && state != "completed" {
					job := actionsJobFixture(73)
					job["head_sha"], job["conclusion"] = candidateMerge, "failure"
					body[key], body["total_count"] = append(rows, job), 2
				}
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.counts[r.URL.Path]++
		f.mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-ci-observation-token" {
			t.Error("optional attention mutated or changed credential binding")
		}
		if mode == "unavailable" {
			http.Error(w, "PRIVATE_OPTIONAL_ERROR", http.StatusServiceUnavailable)
			return
		}
		if mode == "rate limit" {
			w.Header().Set("Retry-After", "42")
			http.Error(w, "PRIVATE_OPTIONAL_ERROR", http.StatusTooManyRequests)
			return
		}
		if mode == "cancel" {
			if optionalStarted != nil {
				optionalStarted()
			}
			<-r.Context().Done()
			return
		}
		run := f.run(60, candidateMerge)
		var body any
		switch r.URL.Path {
		case actionsRunsPath:
			body = actionsEnvelope("workflow_runs", []map[string]any{run}, 1)
		case actionsRunsPath + "/60":
			if mode == "stale attempt" {
				run["run_attempt"] = int64(3)
			}
			body = run
		case actionsRunsPath + "/60/attempts/2/jobs", actionsRunsPath + "/60/attempts/3/jobs":
			jobs := []map[string]any{}
			for _, id := range []int64{92, 90, 91} {
				job := actionsJobFixture(id)
				job["run_id"], job["head_sha"], job["conclusion"] = int64(60), candidateMerge, "failure"
				if id == 91 {
					job["conclusion"] = "skipped"
				}
				jobs = append(jobs, job)
			}
			body = actionsEnvelope("jobs", jobs, len(jobs))
		default:
			t.Error("unexpected optional resource", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.Close)
	f.g = testGitHub(s, "synthetic-ci-observation-token")
	return f
}

func workflowGateFacts(t *testing.T, o CIObservation) any {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err = json.Unmarshal(b, &value); err != nil {
		t.Fatal(err)
	}
	var stripClocks func(any)
	stripClocks = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range []string{"observed_at", "ObservedAt", "started_at"} {
				delete(x, k)
			}
			for _, child := range x {
				stripClocks(child)
			}
		case []any:
			for _, child := range x {
				stripClocks(child)
			}
		}
	}
	stripClocks(value)
	return value
}

func TestObserveCIGateWithWorkflowsKeepsGateAndBoundsExtraReads(t *testing.T) {
	base := ciWorkflowFailureFixture(t, "failure")
	ordinary, err := base.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	if err != nil || ordinary.State != "pending" {
		t.Fatal(ordinary.State, err)
	}
	basePaths, _ := base.snapshot()
	for _, mode := range []string{"default", "overlap", "optional", "missing", "unavailable", "stale attempt", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			optionalStarted := make(chan struct{}, 1)
			f := ciWorkflowFailureFixtureWithOptionalStart(t, mode, func() {
				// Cancel only after the required gate completed and the first
				// optional request reached the server, not after a wall-clock guess.
				optionalStarted <- struct{}{}
				cancel()
			})
			ids := []int64(nil)
			switch mode {
			case "overlap":
				ids = []int64{30}
			case "missing":
				ids = []int64{99}
			case "default":
			default:
				ids = []int64{31}
			}
			got, failures, err := f.g.ObserveCIGateWithWorkflows(ctx, ciObservationTarget(), "main", ids)
			if mode == "cancel" {
				select {
				case <-optionalStarted:
				default:
					t.Fatal("cancellation did not reach optional collection")
				}
			}
			var warning *CIWorkflowAttentionError
			wantWarning := mode == "unavailable" || mode == "cancel"
			if (err != nil) != wantWarning || wantWarning && !errors.As(err, &warning) || !reflect.DeepEqual(workflowGateFacts(t, ordinary), workflowGateFacts(t, got)) || len(got.Attempts) != 1 {
				t.Fatal("optional attention changed required observation", got.State, err)
			}
			paths, counts := f.snapshot()
			if mode == "optional" {
				if len(failures) != 1 || failures[0] != (CIWorkflowFailure{WorkflowID: 31, RunID: 60, RunAttempt: 2, JobID: 90, CheckRunID: 1090, Name: "test [linux]", Conclusion: "failure"}) || len(paths) != len(basePaths)+4 || counts[actionsRunsPath+"/60/attempts/2/jobs"] != 1 {
					t.Fatal("optional workflow was not selected once with its lowest failed job", failures, paths)
				}
			} else if mode == "overlap" {
				if len(failures) != 1 || failures[0].WorkflowID != 30 || failures[0].RunID != 50 || failures[0].JobID != 73 || failures[0].CheckRunID != 1073 {
					t.Fatal("existing required attempt was not reused", failures)
				}
			} else if len(failures) != 0 {
				t.Fatal("unproven or unrequested workflow supplied attention", failures)
			}
			if mode == "default" || mode == "overlap" || mode == "missing" {
				if !reflect.DeepEqual(basePaths, paths) {
					t.Fatal("empty/missing/overlapping selection added provider reads", paths)
				}
			}
			for _, path := range paths {
				if strings.Contains(path, "/logs") || strings.Contains(path, "/check-runs/") {
					t.Fatal("attention fetched individual jobs or logs", path)
				}
			}
		})
	}
}

func TestObserveCIGateWithWorkflowsPreservesRateLimitAndStopsFanout(t *testing.T) {
	base := ciWorkflowFailureFixture(t, "rate limit")
	ordinary, err := base.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	if err != nil || ordinary.State != "pending" {
		t.Fatal(ordinary.State, err)
	}
	f := ciWorkflowFailureFixture(t, "rate limit")
	got, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{30, 31, 32})
	var warning *CIWorkflowAttentionError
	var provider *Error
	if !errors.As(err, &warning) || !errors.As(err, &provider) || provider.Code != "provider_rate_limited" || provider.Status != 429 || provider.RetryAfterSeconds != 42 || strings.Contains(err.Error(), "PRIVATE_OPTIONAL_ERROR") {
		t.Fatal("optional failure lost safe warning/backoff cause", err)
	}
	if len(failures) != 1 || failures[0].WorkflowID != 30 || !reflect.DeepEqual(workflowGateFacts(t, ordinary), workflowGateFacts(t, got)) {
		t.Fatal("warning erased proven attention or required gate", failures, got.State)
	}
	paths, _ := f.snapshot()
	for _, path := range paths {
		if strings.Contains(path, "check_suite_id=42") || strings.Contains(path, "/runs/61") {
			t.Fatal("optional fanout continued after rate limit", path)
		}
	}
}

func TestObserveCIGateWithWorkflowsSkipsDefinitiveGate(t *testing.T) {
	for _, mode := range []string{"passed", "failed gate", "failed and missing", "empty"} {
		t.Run(mode, func(t *testing.T) {
			base := ciWorkflowFailureFixture(t, mode)
			ordinary, err := base.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
			want := map[string]string{"passed": "satisfied", "failed gate": "failed", "failed and missing": "unproven", "empty": "not_required"}[mode]
			if err != nil || ordinary.State != want {
				t.Fatal(err)
			}
			paths, _ := base.snapshot()
			f := ciWorkflowFailureFixture(t, mode)
			got, failures, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
			actual, _ := f.snapshot()
			if err != nil || len(failures) != 0 || !reflect.DeepEqual(paths, actual) || !reflect.DeepEqual(workflowGateFacts(t, ordinary), workflowGateFacts(t, got)) {
				t.Fatal("definitive gate acquired unnecessary attention reads", err, failures, actual)
			}
		})
	}
}

func TestObserveCIGateWithWorkflowsRejectsSelectorsBeforeProvider(t *testing.T) {
	f := ciWorkflowFailureFixture(t, "failure")
	for _, ids := range [][]int64{{0}, {-1}, {30, 30}, make([]int64, 17)} {
		if _, _, err := f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", ids); err == nil {
			t.Fatal("invalid optional selector accepted", ids)
		}
	}
	paths, _ := f.snapshot()
	if len(paths) != 0 {
		t.Fatal("invalid selection reached provider", paths)
	}
}

func TestCIWorkflowFailureProjectionIsCurrentAndFinite(t *testing.T) {
	for _, event := range []string{"pull_request", "push", "workflow_dispatch", "workflow_run"} {
		f := requiredCIFixture()
		run, attempt := f.Actions.Runs[0], f.Attempts[0]
		run.Event, attempt.Event = event, event
		attempt.Jobs[0].Conclusion = "failure"
		if _, ok := projectCIWorkflowFailure(run, attempt); !ok {
			t.Fatal("explicit workflow attention incorrectly supplied required-event policy", event)
		}
	}
	for _, conclusion := range []string{"failure", "timed_out", "action_required", "startup_failure", "cancelled", "stale", "skipped", "neutral", "success"} {
		t.Run(conclusion, func(t *testing.T) {
			f := requiredCIFixture()
			f.Attempts[0].Jobs[0].Conclusion = conclusion
			before := fixtureJSON(t, f)
			got, ok := projectCIWorkflowFailure(f.Actions.Runs[0], f.Attempts[0])
			want := conclusion == "failure" || conclusion == "timed_out" || conclusion == "action_required" || conclusion == "startup_failure"
			if ok != want || !reflect.DeepEqual(before, fixtureJSON(t, f)) || ok && got.Conclusion != conclusion {
				t.Fatal("wrong failure class or modified facts", got, ok)
			}
			if ok {
				b, _ := json.Marshal(got)
				var fields map[string]any
				if json.Unmarshal(b, &fields) != nil || len(fields) != 7 || fields["workflow_id"] == nil || fields["run_id"] == nil || fields["run_attempt"] == nil || fields["job_id"] == nil || fields["check_run_id"] == nil || fields["name"] == nil || fields["conclusion"] == nil {
					t.Fatal("unexpected attention wire shape", string(b))
				}
			}
		})
	}
	for name, mutate := range map[string]func(*ActionsHeadRun, *ActionsAttempt){
		"stale attempt":         func(_ *ActionsHeadRun, a *ActionsAttempt) { a.RunAttempt-- },
		"stale number":          func(_ *ActionsHeadRun, a *ActionsAttempt) { a.RunNumber-- },
		"wrong run":             func(_ *ActionsHeadRun, a *ActionsAttempt) { a.RunID++ },
		"wrong workflow":        func(_ *ActionsHeadRun, a *ActionsAttempt) { a.WorkflowID++ },
		"wrong suite":           func(_ *ActionsHeadRun, a *ActionsAttempt) { a.CheckSuiteID++ },
		"foreign repository":    func(_ *ActionsHeadRun, a *ActionsAttempt) { a.RepositoryID++ },
		"wrong repository name": func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Repository = "other/repo" },
		"foreign head":          func(_ *ActionsHeadRun, a *ActionsAttempt) { a.HeadSHA = strings.Repeat("f", 40) },
		"wrong event":           func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Event = "workflow_dispatch" },
		"unresolved":            func(_ *ActionsHeadRun, a *ActionsAttempt) { a.State = "unproven" },
		"unknown reason":        func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Reason = "ci_attempt_changed" },
		"missing jobs":          func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Jobs = nil },
		"malformed run":         func(r *ActionsHeadRun, _ *ActionsAttempt) { r.ID = 0 },
		"ambiguous job":         func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Jobs = append(a.Jobs, a.Jobs[0]) },
		"ambiguous check":       func(_ *ActionsHeadRun, a *ActionsAttempt) { j := a.Jobs[0]; j.ID++; a.Jobs = append(a.Jobs, j) },
		"foreign job head":      func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Jobs[0].HeadSHA = strings.Repeat("f", 40) },
		"unsafe name":           func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Jobs[0].Name = "unsafe\x00name" },
		"unbounded name":        func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Jobs[0].Name = strings.Repeat("x", 256) },
		"unfinished job": func(_ *ActionsHeadRun, a *ActionsAttempt) {
			a.Jobs[0].Status = "in_progress"
			a.Jobs[0].Conclusion = ""
			a.Jobs[0].CompletedAt = time.Time{}
		},
		"unbounded jobs": func(_ *ActionsHeadRun, a *ActionsAttempt) { a.Jobs = make([]ActionsJob, githubMaxItems+1) },
	} {
		t.Run(name, func(t *testing.T) {
			f := requiredCIFixture()
			r, a := f.Actions.Runs[0], f.Attempts[0]
			a.Jobs[0].Conclusion = "failure"
			mutate(&r, &a)
			if got, ok := projectCIWorkflowFailure(r, a); ok {
				t.Fatal("unsafe optional attention accepted", got)
			}
		})
	}
}
