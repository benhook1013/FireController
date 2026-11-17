package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type actionsRunTestReader struct {
	*actionsDiagnosticTestReader
	readRun  func(context.Context, string, int64, int64, int64) (reviewprovider.ActionsRunObservation, error)
	runCalls atomic.Int64
}

func (r *actionsRunTestReader) ObserveActionsRunAttempt(ctx context.Context, repo string, id, runID, attempt int64) (reviewprovider.ActionsRunObservation, error) {
	r.runCalls.Add(1)
	return r.readRun(ctx, repo, id, runID, attempt)
}
func diagnosticRunFacts(repo string, id, runID, attempt int64) reviewprovider.ActionsRunObservation {
	now := time.Now().UTC()
	run := reviewprovider.ActionsHeadRun{ID: runID, CheckSuiteID: 300, WorkflowID: 301, RunNumber: 10, RunAttempt: attempt, RepositoryID: id, Repository: repo, HeadSHA: strings.Repeat("c", 40), Event: "push", Status: "in_progress", CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	jobs := []reviewprovider.ActionsJob{{ID: 3, CheckRunID: 103, Name: "done", HeadSHA: run.HeadSHA, Status: "completed", Conclusion: "success", StartedAt: now.Add(-3 * time.Hour), CompletedAt: now.Add(-2 * time.Hour)}, {ID: 2, CheckRunID: 102, Name: "pending", HeadSHA: run.HeadSHA, Status: "queued"}, {ID: 1, CheckRunID: 101, Name: "executing", HeadSHA: run.HeadSHA, Status: "in_progress", StartedAt: now.Add(-time.Hour)}}
	return reviewprovider.ActionsRunObservation{Run: run, Jobs: jobs, ObservedAt: now}
}
func runDiagnosticSetup(t *testing.T) (*attemptFixture, core.Scope, *actionsRunTestReader) {
	f, scope, base := diagnosticSetup(t)
	r := &actionsRunTestReader{actionsDiagnosticTestReader: base}
	r.readRun = func(_ context.Context, repo string, id, runID, attempt int64) (reviewprovider.ActionsRunObservation, error) {
		if repo != "owner/repo" || id != 801 || runID != 50 || attempt != 2 {
			t.Error("run scope changed", repo, id, runID, attempt)
		}
		return diagnosticRunFacts(repo, id, runID, attempt), nil
	}
	return f, scope, r
}
func diagnosticStorageCounts(t *testing.T, f *attemptFixture) [4]int {
	return [4]int{attemptCount(t, f.store, `SELECT count(*) FROM events`), attemptCount(t, f.store, `SELECT count(*) FROM receipts`), attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`), attemptCount(t, f.store, `SELECT count(*) FROM review_operations`)}
}
func TestActionsRunDiagnosticWorkerExplicitReadOnlyCountsAndOmissions(t *testing.T) {
	f, scope, r := runDiagnosticSetup(t)
	before := diagnosticStorageCounts(t, f)
	if _, err := f.e.ObserveActionsRunDiagnostic(t.Context(), scope, 50, 2, 10); err == nil {
		t.Fatal("absent workers accepted")
	}
	stop, done := refreshStart(t, f, r)
	defer refreshStop(t, stop, done)
	got, err := f.e.ObserveActionsRunDiagnostic(t.Context(), scope, 50, 2, 1)
	if err != nil || got.Run.ID != 50 || got.Run.RunAttempt != 2 || got.ProjectID != attemptProject || got.Total != 3 || got.Pending != 1 || got.Running != 1 || got.Completed != 1 || got.Omitted != 2 || len(got.Jobs) != 1 || got.Jobs[0].ID != 1 || got.Capacity != "unknown" || got.Completeness != "non_atomic_attempt_jobs" || r.runCalls.Load() != 1 || r.scans.Load() != 0 || r.calls.Load() != 0 {
		t.Fatal(got, err, r.runCalls.Load())
	}
	if diagnosticStorageCounts(t, f) != before {
		t.Fatal("read wrote native state")
	}
	for _, args := range [][3]int64{{0, 2, 10}, {50, 0, 10}, {50, 2, 0}, {50, 2, 21}} {
		if _, err := f.e.ObserveActionsRunDiagnostic(t.Context(), scope, args[0], args[1], int(args[2])); err == nil {
			t.Fatal("invalid inputs accepted", args)
		}
	}
	if r.runCalls.Load() != 1 {
		t.Fatal("invalid inputs performed provider work")
	}
}
func TestActionsRunDiagnosticSharedPermitSurvivesCallerCancellation(t *testing.T) {
	for _, first := range []string{"scan", "run"} {
		t.Run(first, func(t *testing.T) {
			f, scope, r := runDiagnosticSetup(t)
			entered, release := make(chan struct{}), make(chan struct{})
			if first == "run" {
				r.readRun = func(ctx context.Context, _ string, _, _, _ int64) (reviewprovider.ActionsRunObservation, error) {
					close(entered)
					<-release
					return reviewprovider.ActionsRunObservation{}, ctx.Err()
				}
			} else {
				r.observe = func(ctx context.Context, _ string, _ int64, _ time.Duration) (reviewprovider.ActionsLiveness, error) {
					close(entered)
					<-release
					return reviewprovider.ActionsLiveness{}, ctx.Err()
				}
			}
			stop, done := refreshStart(t, f, r)
			defer func() { close(release); refreshStop(t, stop, done) }()
			ctx, cancel := context.WithCancel(t.Context())
			answer := make(chan error, 1)
			go func() {
				var err error
				if first == "run" {
					_, err = f.e.ObserveActionsRunDiagnostic(ctx, scope, 50, 2, 10)
				} else {
					_, err = f.e.ObserveActionsDiagnostic(ctx, scope, 24, 10)
				}
				answer <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("worker not entered")
			}
			cancel()
			select {
			case err := <-answer:
				if err == nil {
					t.Fatal("cancel credited result")
				}
			case <-time.After(time.Second):
				t.Fatal("caller cancellation unbounded")
			}
			if _, err := f.e.ObserveActionsRunDiagnostic(t.Context(), scope, 50, 2, 10); err == nil {
				t.Fatal("cancelled caller released run permit before provider returned")
			}
			if _, err := f.e.ObserveActionsDiagnostic(t.Context(), scope, 24, 10); err == nil {
				t.Fatal("other mode escaped global permit")
			}
			receipt := refreshEnqueue(t, f, "run-diagnostic-source-exclusion", refreshRequest())
			worked, err := f.e.refreshNextScheduled(t.Context(), map[string]TargetReader{"test-capability": r}, newSourceRefreshSchedule())
			if err != nil || worked || refreshRead(t, f, receipt.ID).State != "queued" {
				t.Fatal("same-project source claim escaped", worked, err)
			}
			// Release the deliberately noncooperative controlled provider before stop.
			release <- struct{}{}
			if r.scans.Load()+r.runCalls.Load() != 1 {
				t.Fatal("diagnostic repeated", r.scans.Load(), r.runCalls.Load())
			}
		})
	}
}
func TestActionsRunDiagnosticCurrentBindingRecheck(t *testing.T) {
	for _, kind := range []string{"repository", "configuration", "read_removed", "revoked", "restore_hold"} {
		t.Run(kind, func(t *testing.T) {
			f, scope, r := runDiagnosticSetup(t)
			r.readRun = func(_ context.Context, repo string, id, runID, attempt int64) (reviewprovider.ActionsRunObservation, error) {
				switch kind {
				case "repository":
					admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
				case "configuration":
					f.configure(t, attemptProject, "test-capability", 1, true)
				case "read_removed":
					admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`)
				case "revoked":
					admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
				case "restore_hold":
					admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('restore:provider_state','held')`)
				}
				return diagnosticRunFacts(repo, id, runID, attempt), nil
			}
			stop, done := refreshStart(t, f, r)
			got, err := f.e.ObserveActionsRunDiagnostic(t.Context(), scope, 50, 2, 10)
			refreshStop(t, stop, done)
			if err == nil || !reflect.DeepEqual(got, ActionsRunDiagnostic{}) || r.runCalls.Load() != 1 {
				t.Fatal("stale authority returned facts", got, err)
			}
		})
	}
}
func TestActionsRunDiagnosticRefusesForgedFactsWithoutPartialRows(t *testing.T) {
	for _, kind := range []string{"failure", "repository", "run", "attempt", "head", "event", "state", "nil_jobs", "duplicate_job", "duplicate_check", "job_head", "job_state", "job_name", "job_time", "stale_result", "future_result"} {
		t.Run(kind, func(t *testing.T) {
			f, scope, r := runDiagnosticSetup(t)
			r.readRun = func(_ context.Context, repo string, id, runID, attempt int64) (reviewprovider.ActionsRunObservation, error) {
				v := diagnosticRunFacts(repo, id, runID, attempt)
				switch kind {
				case "failure":
					return v, errors.New("PRIVATE_PROVIDER_FAILURE")
				case "repository":
					v.Run.RepositoryID++
				case "run":
					v.Run.ID++
				case "attempt":
					v.Run.RunAttempt++
				case "head":
					v.Run.HeadSHA = "wrong"
				case "event":
					v.Run.Event = "bad\nevent"
				case "state":
					v.Run.Status = "unknown"
				case "nil_jobs":
					v.Jobs = nil
				case "duplicate_job":
					v.Jobs[1].ID = v.Jobs[0].ID
				case "duplicate_check":
					v.Jobs[1].CheckRunID = v.Jobs[0].CheckRunID
				case "job_head":
					v.Jobs[0].HeadSHA = strings.Repeat("d", 40)
				case "job_state":
					v.Jobs[0].Status = "unknown"
				case "job_name":
					v.Jobs[0].Name = "bad\nname"
				case "job_time":
					v.Jobs[2].StartedAt = time.Time{}
				case "stale_result":
					v.ObservedAt = time.Now().Add(-time.Hour)
				case "future_result":
					v.ObservedAt = time.Now().Add(time.Hour)
				}
				return v, nil
			}
			stop, done := refreshStart(t, f, r)
			got, err := f.e.ObserveActionsRunDiagnostic(t.Context(), scope, 50, 2, 10)
			refreshStop(t, stop, done)
			if err == nil || !reflect.DeepEqual(got, ActionsRunDiagnostic{}) || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("partial/forged facts survived", got, err)
			}
		})
	}
}
func TestActionsRunDiagnosticByteBoundStableOrderAndCanonicalSkippedTimes(t *testing.T) {
	facts := diagnosticRunFacts("owner/repo", 801, 50, 2)
	request := &actionsDiagnosticRequest{started: time.Now().Add(-time.Second), binding: actionsDiagnosticBinding{project: attemptProject, repository: "owner/repo", repositoryID: 801}, runID: 50, attempt: 2, limit: 20}
	facts.Jobs = nil
	for i := int64(20); i > 0; i-- {
		facts.Jobs = append(facts.Jobs, reviewprovider.ActionsJob{ID: i, CheckRunID: i + 100, HeadSHA: facts.Run.HeadSHA, Name: strings.Repeat("<", 255), Status: "completed", Conclusion: "skipped", StartedAt: facts.ObservedAt.Add(time.Minute), CompletedAt: facts.ObservedAt.Add(-time.Minute)})
	}
	got, err := projectActionsRunDiagnostic(request, facts)
	raw, _ := json.Marshal(got)
	if err != nil || len(raw)+1 > actionsDiagnosticBytes || len(got.Jobs) == 0 || len(got.Jobs) >= 20 || got.Completed != 20 || got.Omitted != 20-len(got.Jobs) {
		t.Fatal(len(raw), got, err)
	}
	for i, job := range got.Jobs {
		if job.ID != int64(i+1) || job.StartedAt != actionsRunTime(facts.Jobs[0].StartedAt) {
			t.Fatal("ordering/timestamps changed", got)
		}
	}
}

func TestActionsRunDiagnosticHTTPReadScopeAndStrictInputs(t *testing.T) {
	f, _, reader := runDiagnosticSetup(t)
	cancel, done := refreshStart(t, f, reader)
	defer refreshStop(t, cancel, done)
	mux := http.NewServeMux()
	f.e.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	for _, body := range []string{`{}`, `null`, `{"run_id":0,"run_attempt":2,"limit":10}`, `{"run_id":50,"run_attempt":2,"limit":21}`, `{"run_id":50,"run_attempt":2,"limit":10,"repository":"foreign/repo"}`, `{"run_id":50,"run_id":51,"run_attempt":2,"limit":10}`, `{"run_id":50,"run_attempt":null,"limit":10}`} {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/projects/"+attemptProject+"/reviews/actions-run/observe", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+f.reader)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatal("invalid input accepted", body, response.StatusCode)
		}
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/projects/"+otherAttemptProject+"/reviews/actions-run/observe", strings.NewReader(`{"run_id":50,"run_attempt":2,"limit":10}`))
	request.Header.Set("Authorization", "Bearer "+f.reader)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || reader.runCalls.Load() != 0 {
		t.Fatal("foreign project reached provider", response.StatusCode, reader.runCalls.Load())
	}
	request, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/projects/"+attemptProject+"/reviews/actions-run/observe", strings.NewReader(`{"run_id":50,"run_attempt":2,"limit":10}`))
	request.Header.Set("Authorization", "Bearer "+f.reader)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got ActionsRunDiagnostic
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&got) != nil || got.ProjectID != attemptProject || reader.runCalls.Load() != 1 {
		t.Fatal("bound read grant failed", response.StatusCode, got, reader.runCalls.Load())
	}
}
