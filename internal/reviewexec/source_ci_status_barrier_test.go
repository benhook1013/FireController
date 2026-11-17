package reviewexec

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func sourceCIAppStatusObservation(target reviewprovider.CITarget, status string, legacy bool) reviewprovider.CIObservation {
	o := sourceCITestObservation(target, "satisfied")
	now := o.ObservedAt
	o.Authority.Checks[0].AppID = 15368
	o.Results.Statuses[0].State = status
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

func TestSourceCIAppStatusBarrierFreshAndExactLegacyRetention(t *testing.T) {
	for _, status := range []string{"success", "pending", "failure"} {
		for _, legacy := range []bool{false, true} {
			name := status
			if legacy {
				name += "/retained_legacy"
			}
			t.Run(name, func(t *testing.T) {
				f := newAttemptFixture(t)
				f.configure(t, attemptProject, "test-capability", 0, false)
				var collected reviewprovider.CIObservation
				reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
					collected = sourceCIAppStatusObservation(target, status, false)
					return collected, nil
				}}
				receipt := refreshEnqueue(t, f, "app-status", sourceCIRequest())
				cancel, done := refreshStart(t, f, reader)
				got := refreshWait(t, f, receipt.ID, "completed")
				refreshStop(t, cancel, done)
				want := map[string]string{"success": "satisfied", "pending": "pending", "failure": "failed"}[status]
				if got.CI == nil || got.CI.State != want || got.CI.Required != 1 {
					t.Fatal("new semantics not ingested", got.CI)
				}
				if legacy {
					// Exact canonical pre-change assessment over these same immutable facts.
					collected.Assessment.State, collected.Assessment.Reason = "unproven", "ci_required_checks_unproven"
					collected.Assessment.Checks[0].State, collected.Assessment.Checks[0].Reason = "unproven", "ci_status_origin_unproven"
					collected.State, collected.Reason = collected.Assessment.State, collected.Assessment.Reason
					if validSourceCI(collected, *got.Observation) || !validSourceCIWithRetainedAssessment(collected, *got.Observation, true) {
						t.Fatal("fresh/retained validation boundary lost")
					}
					admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, encoded(collected), receipt.ID)
					want = "unproven"
				}
				before := sourceCITestState(t, f)
				for i := 0; i < 2; i++ {
					if i == 1 {
						var err error
						f.e, err = New(f.store, attemptCapabilities)
						if err != nil {
							t.Fatal(err)
						}
					}
					operation, err := f.e.ReadSourceRefresh(t.Context(), sourceCIScope(t, f), receipt.ID)
					if err != nil || operation.CI == nil || operation.CI.State != want {
						t.Fatal("retained source read/restart failed", operation.CI, err)
					}
					read := readSourceCI(t, f)
					if read.State != want || read.Required != 1 {
						t.Fatal("summary state changed", read)
					}
					if legacy && (read.Reason != "ci_status_origin_unproven" || read.Unproven != 1 || read.Satisfied != 0 || read.Pending != 0 || read.Failed != 0) {
						t.Fatal("legacy counts rewritten", read)
					}
					if !legacy && status == "success" && read.Satisfied != 1 {
						t.Fatal("native and supplementary success lost")
					}
					if !legacy && status == "pending" && read.Pending != 1 {
						t.Fatal("pending barrier lost")
					}
					if !legacy && status == "failure" && read.Failed != 1 {
						t.Fatal("failure barrier lost")
					}
					if _, err = f.e.ReadSourcePolicyAssessment(t.Context(), sourceCIScope(t, f), 2); err != nil {
						t.Fatal("CI decode blocked downstream policy read", err)
					}
				}
				if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
					t.Fatal("retained read rewrote observations or contacted collector")
				}
				if legacy {
					raw := encoded(collected)
					var tampered reviewprovider.CIObservation
					if json.Unmarshal([]byte(raw), &tampered) != nil {
						t.Fatal("fixture")
					}
					tampered.Assessment.Checks[0].StatusID++
					admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, encoded(tampered), receipt.ID)
					if _, err := f.e.ReadSourceRefresh(t.Context(), sourceCIScope(t, f), receipt.ID); err == nil {
						t.Fatal("tampered legacy assessment accepted")
					}
					if reflect.DeepEqual(collected.Assessment, tampered.Assessment) {
						t.Fatal("tamper fixture did not change")
					}
				}
			})
		}
	}
}

func TestSourceCIFreshLegacyOriginAssessmentIsNotAdopted(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return sourceCIAppStatusObservation(target, "success", true), nil
	}}
	receipt := refreshEnqueue(t, f, "fake-legacy", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.CI == nil || got.CI.State != "unproven" || got.CI.Reason != "ci_observation_unproven" || got.CI.Required != 0 {
		t.Fatal("new source accepted legacy collector assessment", got.CI)
	}
}
