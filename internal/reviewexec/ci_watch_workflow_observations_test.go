package reviewexec

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestCIWatchWorkflowObservationsLatestRunOnlyAndOriginalFacts(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	o := sourceCITestObservation(target, "pending")
	run := reviewprovider.ActionsHeadRun{ID: 100, WorkflowID: 10, CheckSuiteID: 1000, RunNumber: 1, RunAttempt: 3, RepositoryID: target.RepositoryID, Repository: target.Repository, HeadSHA: o.SelectedCommitSHA, Event: "pull_request", Status: "completed", Conclusion: "failure", CreatedAt: o.ObservedAt.Add(-time.Hour), UpdatedAt: o.ObservedAt.Add(-time.Hour)}
	newer := run
	newer.ID, newer.CheckSuiteID, newer.RunNumber, newer.RunAttempt = 101, 1001, 2, 1
	newer.Conclusion = "success"
	newer.CreatedAt, newer.UpdatedAt = o.ObservedAt.Add(-time.Minute), o.ObservedAt
	o.Actions.Runs = append(o.Actions.Runs, run, newer)
	before := encoded(o)
	got := ciWatchWorkflowObservations(o, []int64{20, 10})
	want := []CIWorkflowObservation{{WorkflowID: 10, Basis: "latest_run_only", RunID: 101, RunAttempt: 1, Status: "completed", Conclusion: "success"}, {WorkflowID: 20, Basis: "latest_run_only"}}
	if !reflect.DeepEqual(got, want) || before != encoded(o) {
		t.Fatal("latest-run selection guessed history/job proof or rewrote facts", got)
	}
	for _, damage := range []string{"candidate", "repository", "head", "coherence"} {
		copy := o
		actions := *o.Actions
		copy.Actions = &actions
		candidate := *o.Candidate
		copy.Candidate = &candidate
		switch damage {
		case "candidate":
			candidate.State = "unproven"
		case "repository":
			actions.RepositoryID++
		case "head":
			actions.HeadSHA = target.BaseSHA
		case "coherence":
			copy.Reason = "ci_observation_changed"
		}
		if ciWatchWorkflowObservations(copy, []int64{10}) != nil {
			t.Fatal("unproven current facts supplied workflow observation", damage)
		}
	}
	// Display-only metadata cannot change the failure baseline or wake state.
	baseKey := strings.Repeat("d", 64)
	item := CIWatchItem{CIWatchTarget: CIWatchTarget{PullNumber: 42, HeadSHA: target.HeadSHA}, State: "pending", Reason: "ci_required_checks_pending", ObservedHeadSHA: target.HeadSHA, ObservedBaseSHA: target.BaseSHA, CICommitSHA: target.HeadSHA, Selection: "head", ObservationKey: baseKey, Checks: []SourceCIContext{}, WorkflowObservations: got}
	entry := &ciWatchEntry{version: 1, wanted: 1, done: 1, result: item}
	s := newLifecycleSchedule(LifecycleSyncOptions{Now: time.Now})
	sub := &ciWatchSubscription{scope: core.Scope{ProjectID: "one"}, targets: []CIWatchTarget{{PullNumber: 42, HeadSHA: target.HeadSHA, FailureWorkflowIDs: []int64{10}}}, minimum: []uint64{1}, bases: []string{target.BaseSHA}, entries: []*ciWatchEntry{entry}}
	result := s.ciWatchResult(sub)
	if result.State != "pending" || len(result.Targets[0].WorkflowObservations) != 1 || result.Targets[0].WorkflowObservations[0].WorkflowID != 10 || len(result.Targets[0].WorkflowFailures) != 0 {
		t.Fatal("display observation became attention or leaked another selection", result)
	}
	key := result.Targets[0].ObservationKey
	entry.result.WorkflowObservations[0].Status = "in_progress"
	if s.ciWatchResult(sub).Targets[0].ObservationKey != key {
		t.Fatal("display-only metadata changed baseline")
	}
	for _, state := range []string{"passed", "failed", "closed", "superseded", "unconfigured"} {
		entry.result.State = state
		if len(s.ciWatchResult(sub).Targets[0].WorkflowObservations) != 0 {
			t.Fatal("terminal gate exposed optional observation", state)
		}
	}
	entry.result.State = "pending"
	entry.wanted = 2
	if len(s.ciWatchResult(sub).Targets[0].WorkflowObservations) != 0 {
		t.Fatal("refreshing result exposed old optional facts")
	}
	entry.wanted = 1
	sub.minimum[0] = 2
	if len(s.ciWatchResult(sub).Targets[0].WorkflowObservations) != 0 {
		t.Fatal("old generation exposed optional facts")
	}
}

func TestCIWatchWorkflowObservationsHTTPOptInAndNoExtraCollection(t *testing.T) {
	f, s, _, base, _ := ciWatchFixture(t)
	base.states[42] = "pending"
	r := &ciWatchWorkflowReader{ciWatchReader: base, conclusions: map[int64]string{10: "failure"}}
	s.readers["test-capability"] = r
	before := lifecycleHintCounts(t, f)
	for _, header := range []string{"", "unknown", "1"} {
		req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(`{"targets":[{"pull_number":42,"head_sha":"`+strings.Repeat("a", 40)+`","failure_workflow_ids":[10]}]}`))
		req.Header.Set("Authorization", "Bearer "+f.reader)
		req.Header.Set("Prefer", "wait=5")
		req.Header.Set("X-FC-CI-Watch", "change-v1")
		req.Header.Set("X-FC-CI-Workflow-Details", header)
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { f.handler.ServeHTTP(w, req); close(done) }()
		ciWatchRegistered(t, s, 1)
		watchTick(t, f, s)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("watch did not return attention")
		}
		var value CIWatchResult
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil || value.State != "attention" {
			t.Fatal(w.Code, w.Body.String())
		}
		if (len(value.Targets[0].WorkflowObservations) > 0) != (header == "1") || strings.Contains(w.Body.String(), `"workflow_observations"`) != (header == "1") {
			t.Fatal("optional detail escaped negotiated wire", header, w.Body.String())
		}
		if header == "1" && value.Targets[0].WorkflowObservations[0].RunID != 100 {
			t.Fatal("wrong run identity")
		}
		ciWatchRegistered(t, s, 0)
	}
	if base.calls != 3 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("visibility added collection or mutated project state", base.calls)
	}
}

func TestCIWatchFallbackOmitsWholeDetailGroupOnlyForAffectedSubscriber(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	entry := &ciWatchEntry{version: 1, wanted: 1, done: 1, result: CIWatchItem{
		State: "pending", Reason: "ci_required_checks_pending", ObservedHeadSHA: head, ObservedBaseSHA: base, CICommitSHA: head, Selection: "head", ObservationKey: strings.Repeat("d", 64), Checks: []SourceCIContext{},
		WorkflowObservations: []CIWorkflowObservation{{WorkflowID: 10, Basis: "latest_run_only", RunID: 101, RunAttempt: 1}, {WorkflowID: 20, Basis: "latest_run_only", RunID: 200, RunAttempt: 1}},
		WorkflowFailures:     []reviewprovider.CIWorkflowFailure{{WorkflowID: 10, RunID: 100, RunAttempt: 1}},
	}}
	before := encoded(entry.result)
	s := newLifecycleSchedule(LifecycleSyncOptions{Now: time.Now})
	sub := &ciWatchSubscription{scope: core.Scope{ProjectID: "one"}, targets: []CIWatchTarget{{PullNumber: 42, HeadSHA: head, FailureWorkflowIDs: []int64{10, 20}}}, minimum: []uint64{1}, bases: []string{base}, entries: []*ciWatchEntry{entry}}
	got := s.ciWatchResult(sub)
	if got.State != "attention" || len(got.Targets[0].WorkflowObservations) != 0 || len(got.Targets[0].WorkflowFailures) != 1 || got.Targets[0].WorkflowFailures[0].RunID != 100 {
		t.Fatal("fallback must omit the entire optional group, preserving attention", got)
	}
	sub.targets[0].FailureWorkflowIDs = []int64{20}
	other := s.ciWatchResult(sub)
	if other.State != "pending" || len(other.Targets[0].WorkflowFailures) != 0 || len(other.Targets[0].WorkflowObservations) != 1 || other.Targets[0].WorkflowObservations[0].RunID != 200 || encoded(entry.result) != before {
		t.Fatal("one subscriber's omission erased another subscriber's latest facts", other)
	}
}
