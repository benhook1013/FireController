package reviewexec

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func providerRetainedCIObservation(target reviewprovider.CITarget, legacy bool) reviewprovider.CIObservation {
	o := sourceCITestObservation(target, "satisfied")
	now := o.ObservedAt
	o.Authority.Checks[0].AppID = 15368
	check := reviewprovider.CICheckRun{ID: 300, SuiteID: 90, AppID: 15368, Name: "build", HeadSHA: target.HeadSHA, Status: "completed", Conclusion: "success", StartedAt: now.Add(-time.Minute), CompletedAt: now}
	run := reviewprovider.ActionsHeadRun{ID: 80, CheckSuiteID: 90, WorkflowID: 100, RunNumber: 12, RunAttempt: 2, RepositoryID: target.RepositoryID, Repository: target.Repository, HeadSHA: target.HeadSHA, Event: "pull_request", Status: "completed", Conclusion: "success", CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	job := reviewprovider.ActionsJob{ID: 500, CheckRunID: 300, Name: "build", HeadSHA: target.HeadSHA, Status: "completed", Conclusion: "success", StartedAt: check.StartedAt, CompletedAt: now}
	o.Results.Checks = []reviewprovider.CICheckRun{check}
	o.Actions.Runs = []reviewprovider.ActionsHeadRun{run}
	o.Attempts = []reviewprovider.ActionsAttempt{{State: "complete", Repository: target.Repository, RepositoryID: target.RepositoryID, HeadSHA: target.HeadSHA, CheckSuiteID: 90, RunID: 80, WorkflowID: 100, RunNumber: 12, RunAttempt: 2, Event: "pull_request", Status: "completed", Conclusion: "success", ObservedAt: now, Jobs: []reviewprovider.ActionsJob{job}}}
	o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
	if legacy {
		o.Assessment.State, o.Assessment.Reason = "unproven", "ci_required_checks_unproven"
		o.Assessment.Checks[0].State, o.Assessment.Checks[0].Reason = "unproven", "ci_status_origin_unproven"
	}
	o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	return o
}

func TestProviderRetainedCIExactOriginHoldAndCurrentAssessment(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	var current reviewprovider.CIObservation
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		current = providerRetainedCIObservation(target, false)
		return current, nil
	}}
	receipt := refreshEnqueue(t, f, "provider-current-ci", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.CI == nil || got.CI.State != "satisfied" || got.CI.Satisfied != 1 {
		t.Fatal("current assessment was not collected", got.CI)
	}
	// Reproduce the exact canonical assessment stored by the parent adapter,
	// retaining its original provider inventories and conservative outcome.
	legacy := current
	legacy.Assessment.Checks = append([]reviewprovider.RequiredCIAssessmentCheck(nil), current.Assessment.Checks...)
	legacy.Assessment.State, legacy.Assessment.Reason = "unproven", "ci_required_checks_unproven"
	legacy.Assessment.Checks[0].State, legacy.Assessment.Checks[0].Reason = "unproven", "ci_status_origin_unproven"
	legacy.State, legacy.Reason = legacy.Assessment.State, legacy.Assessment.Reason
	if validSourceCI(legacy, *got.Observation) {
		t.Fatal("fresh collection accepted an obsolete origin hold")
	}
	admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, encoded(legacy), receipt.ID)
	before := sourceCITestState(t, f)
	for i := 0; i < 2; i++ {
		if i == 1 {
			var err error
			f.e, err = New(f.store, attemptCapabilities)
			if err != nil {
				t.Fatal("restart rejected original CI facts", err)
			}
		}
		operation := refreshRead(t, f, receipt.ID)
		assessment := readSourceCI(t, f)
		if operation.CI == nil || operation.CI.State != "unproven" || assessment.Unproven != 1 || assessment.Satisfied != 0 || assessment.State != "unproven" {
			t.Fatal("retained conservative outcome changed", operation.CI, assessment)
		}
	}
	if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
		t.Fatal("local reads changed retained facts or fetched provider evidence")
	}
	for _, field := range []string{"status identity", "reason", "current job identity", "source head"} {
		t.Run(field, func(t *testing.T) {
			var changed reviewprovider.CIObservation
			if err := json.Unmarshal([]byte(encoded(legacy)), &changed); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "status identity":
				changed.Assessment.Checks[0].StatusID++
			case "reason":
				changed.Assessment.Checks[0].Reason = "ci_unknown"
			case "current job identity":
				changed.Attempts[0].Jobs[0].CheckRunID++
			case "source head":
				changed.Target.HeadSHA = changed.Target.BaseSHA
			}
			admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, encoded(changed), receipt.ID)
			if _, err := f.e.ReadSourceRefresh(t.Context(), sourceCIScope(t, f), receipt.ID); err == nil {
				t.Fatal("changed legacy facts accepted")
			}
		})
	}
	admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, encoded(current), receipt.ID)
	if got := readSourceCI(t, f); got.State != "satisfied" || got.Satisfied != 1 {
		t.Fatal("current assessment no longer valid", got)
	}
}

func TestProviderRetainedCIFreshOriginHoldIsNotAdopted(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return providerRetainedCIObservation(target, true), nil
	}}
	receipt := refreshEnqueue(t, f, "provider-fake-legacy-ci", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.CI == nil || got.CI.State != "unproven" || got.CI.Reason != "ci_observation_unproven" || got.CI.Required != 0 || reader.ciCalls.Load() != 1 {
		t.Fatal("fresh origin hold was accepted as retained evidence", got.CI)
	}
}

func TestProviderRetainedCIExactSameSuiteAmbiguity(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	var current reviewprovider.CIObservation
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		current = providerRetainedCIObservation(target, false)
		old := current.Results.Checks[0]
		old.ID = 301
		old.Conclusion = "failure"
		old.CompletedAt = old.CompletedAt.Add(-time.Hour)
		old.StartedAt = old.CompletedAt.Add(-time.Minute)
		current.Results.Checks = append(current.Results.Checks, old)
		current.Assessment, _ = reviewprovider.AssessRequiredCI(current.Authority, *current.Results, *current.Actions, current.Attempts)
		current.State, current.Reason = current.Assessment.State, current.Assessment.Reason
		return current, nil
	}}
	receipt := refreshEnqueue(t, f, "provider-rerun-ci", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.CI == nil || got.CI.State != "satisfied" {
		t.Fatal("current rerun not collected", got.CI)
	}
	original := current
	original.Assessment.Checks = append([]reviewprovider.RequiredCIAssessmentCheck(nil), current.Assessment.Checks...)
	original.Assessment.State, original.Assessment.Reason = "unproven", "ci_required_checks_unproven"
	check := &original.Assessment.Checks[0]
	check.State, check.Reason = "unproven", "ci_check_ambiguous"
	check.CheckRunID, check.RunID, check.RunAttempt = 0, 0, 0
	original.State, original.Reason = original.Assessment.State, original.Assessment.Reason
	if validSourceCI(original, *got.Observation) {
		t.Fatal("fresh collection adopted parent ambiguity")
	}
	admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, encoded(original), receipt.ID)
	before := sourceCITestState(t, f)
	for i := 0; i < 2; i++ {
		if i == 1 {
			var err error
			f.e, err = New(f.store, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
		}
		operation := refreshRead(t, f, receipt.ID)
		assessment := readSourceCI(t, f)
		if operation.CI == nil || operation.CI.State != "unproven" || assessment.State != "unproven" || assessment.Unproven != 1 || assessment.Satisfied != 0 {
			t.Fatal("original ambiguity changed", operation.CI, assessment)
		}
	}
	if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
		t.Fatal("retained bytes changed or provider fetched")
	}
	original.Assessment.Checks[0].CheckRunID = 300
	admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, encoded(original), receipt.ID)
	if _, err := f.e.ReadSourceRefresh(t.Context(), sourceCIScope(t, f), receipt.ID); err == nil {
		t.Fatal("invented parent check identity accepted")
	}
}
