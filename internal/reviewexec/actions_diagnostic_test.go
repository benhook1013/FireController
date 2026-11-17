package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type actionsDiagnosticTestReader struct {
	refreshTestReader
	observe func(context.Context, string, int64, time.Duration) (reviewprovider.ActionsLiveness, error)
	scans   atomic.Int64
}

func (r *actionsDiagnosticTestReader) ObserveActionsLiveness(ctx context.Context, repo string, id int64, age time.Duration) (reviewprovider.ActionsLiveness, error) {
	r.scans.Add(1)
	return r.observe(ctx, repo, id, age)
}
func diagnosticFacts(repo string, id int64, count int) reviewprovider.ActionsLiveness {
	now := time.Now().UTC()
	out := reviewprovider.ActionsLiveness{Repository: repo, RepositoryID: id, ObservedAt: now, Runs: []reviewprovider.ActionsLivenessRun{}}
	for i := 0; i < count; i++ {
		row := reviewprovider.ActionsHeadRun{ID: int64(i + 1), CheckSuiteID: int64(i + 1001), WorkflowID: 30, RunNumber: int64(i + 1), RunAttempt: 2, RepositoryID: id, Repository: repo, HeadSHA: strings.Repeat("c", 40), Event: "pull_request", Status: "queued", CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-48*time.Hour + time.Duration(i)*time.Second)}
		out.Runs = append(out.Runs, reviewprovider.ActionsLivenessRun{ActionsHeadRun: row, Aged: true, Capacity: "unknown"})
	}
	return out
}
func diagnosticSetup(t *testing.T) (*attemptFixture, core.Scope, *actionsDiagnosticTestReader) {
	t.Helper()
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, true)
	reader := &actionsDiagnosticTestReader{observe: func(_ context.Context, repo string, id int64, age time.Duration) (reviewprovider.ActionsLiveness, error) {
		if repo != "owner/repo" || id != 801 || age != 24*time.Hour {
			t.Error("caller selected provider scope", repo, id, age)
		}
		return diagnosticFacts(repo, id, 2), nil
	}}
	return f, evidenceReadScope(t, f, attemptProject, f.reader), reader
}
func TestActionsDiagnosticWorkerIsExplicitReadOnlyAndBounded(t *testing.T) {
	f, scope, reader := diagnosticSetup(t)
	beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	beforeReceipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	if _, err := f.e.ObserveActionsDiagnostic(t.Context(), scope, 24, 10); err == nil {
		t.Fatal("absent workers accepted")
	}
	cancel, done := refreshStart(t, f, reader)
	got, err := f.e.ObserveActionsDiagnostic(t.Context(), scope, 24, 1)
	if err != nil || got.ProjectID != attemptProject || got.RepositoryID != 801 || got.Scanned != 2 || got.Aged != 2 || got.Omitted != 1 || len(got.Runs) != 1 || got.Runs[0].ID != 1 || got.Capacity != "unknown" || got.Completeness != "non_atomic_status_buckets" || reader.scans.Load() != 1 || reader.calls.Load() != 0 {
		t.Fatal(got, err, reader.scans.Load(), reader.calls.Load())
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != beforeReceipts || attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("read created native action, event or source refresh")
	}
	refreshStop(t, cancel, done)
}
func TestActionsDiagnosticOneSlotCancellationAndSourceProjectExclusion(t *testing.T) {
	f, scope, reader := diagnosticSetup(t)
	entered, exited := make(chan struct{}), make(chan struct{})
	reader.observe = func(ctx context.Context, _ string, _ int64, _ time.Duration) (reviewprovider.ActionsLiveness, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return reviewprovider.ActionsLiveness{}, ctx.Err()
	}
	stop, done := refreshStart(t, f, reader)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		facts ActionsDiagnostic
		err   error
	}
	answer := make(chan result, 1)
	go func() { facts, err := f.e.ObserveActionsDiagnostic(ctx, scope, 24, 10); answer <- result{facts, err} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("explicit worker not entered")
	}
	if _, err := f.e.ObserveActionsDiagnostic(t.Context(), scope, 24, 10); err == nil {
		t.Fatal("second diagnostic escaped shared admission")
	}
	receipt := refreshEnqueue(t, f, "diagnostic-project-exclusion", refreshRequest())
	worked, err := f.e.refreshNextScheduled(t.Context(), map[string]TargetReader{"test-capability": reader}, newSourceRefreshSchedule())
	if err != nil || worked || reader.calls.Load() != 0 || refreshRead(t, f, receipt.ID).State != "queued" {
		t.Fatal("diagnostic bypassed project source serialization", worked, err)
	}
	cancel()
	select {
	case a := <-answer:
		if a.err == nil || !reflect.DeepEqual(a.facts, ActionsDiagnostic{}) {
			t.Fatal("cancellation credited facts", a)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation unbounded")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("provider read not cancelled")
	}
	refreshStop(t, stop, done)
	if reader.scans.Load() != 1 {
		t.Fatal("cancellation retried", reader.scans.Load())
	}
	// No interrupted diagnostic is replayed on restart. Pending ordinary source
	// work keeps its existing, separate restart semantics.
	reader.observe = func(_ context.Context, repo string, id int64, _ time.Duration) (reviewprovider.ActionsLiveness, error) {
		return diagnosticFacts(repo, id, 0), nil
	}
	restart, restarted := refreshStart(t, f, reader)
	refreshWait(t, f, receipt.ID, "completed")
	if reader.scans.Load() != 1 {
		t.Fatal("restart automatically replayed a diagnostic", reader.scans.Load())
	}
	got, err := f.e.ObserveActionsDiagnostic(t.Context(), scope, 24, 10)
	if err != nil || got.Scanned != 0 || reader.scans.Load() != 2 {
		t.Fatal("restart replayed diagnostic", got, err, reader.scans.Load())
	}
	refreshStop(t, restart, restarted)
}
func TestActionsDiagnosticFreshBindingAndAuthorityRechecked(t *testing.T) {
	for _, change := range []string{"configuration", "repository", "revoked", "read_grant", "restore_hold"} {
		t.Run(change, func(t *testing.T) {
			f, scope, reader := diagnosticSetup(t)
			reader.observe = func(_ context.Context, repo string, id int64, _ time.Duration) (reviewprovider.ActionsLiveness, error) {
				switch change {
				case "configuration":
					f.configure(t, attemptProject, "other-account", 1, true)
				case "repository":
					admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
				case "revoked":
					admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
				case "read_grant":
					admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`)
				case "restore_hold":
					admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('restore:provider_state','held')`)
				}
				return diagnosticFacts(repo, id, 1), nil
			}
			cancel, done := refreshStart(t, f, reader)
			got, err := f.e.ObserveActionsDiagnostic(t.Context(), scope, 24, 10)
			if err == nil || !reflect.DeepEqual(got, ActionsDiagnostic{}) || reader.scans.Load() != 1 {
				t.Fatal("stale scoped facts returned", got, err)
			}
			refreshStop(t, cancel, done)
		})
	}
}
func TestActionsDiagnosticProviderFailureAndForgedFactsRetainNoPartialResult(t *testing.T) {
	for _, kind := range []string{"failure", "repository", "nil_rows", "future", "identity", "number", "state", "head", "capacity", "stale_result", "event", "forged_age"} {
		t.Run(kind, func(t *testing.T) {
			f, scope, reader := diagnosticSetup(t)
			reader.observe = func(_ context.Context, repo string, id int64, _ time.Duration) (reviewprovider.ActionsLiveness, error) {
				facts := diagnosticFacts(repo, id, 2)
				switch kind {
				case "failure":
					return facts, errors.New("PRIVATE_PROVIDER_FAILURE")
				case "repository":
					facts.RepositoryID++
				case "nil_rows":
					facts.Runs = nil
				case "future":
					facts.ObservedAt = time.Now().Add(time.Hour)
				case "identity":
					facts.Runs[1].ID = facts.Runs[0].ID
				case "number":
					facts.Runs[1].RunNumber = facts.Runs[0].RunNumber
				case "state":
					facts.Runs[0].Status = "completed"
				case "head":
					facts.Runs[0].HeadSHA = strings.Repeat("C", 40)
				case "capacity":
					facts.Runs[0].Capacity = "free"
				case "event":
					facts.Runs[0].Event = "private\ntext"
				case "forged_age":
					facts.Runs[0].UpdatedAt = facts.ObservedAt
				case "stale_result":
					facts.ObservedAt = time.Now().Add(-time.Hour)
				}
				return facts, nil
			}
			cancel, done := refreshStart(t, f, reader)
			got, err := f.e.ObserveActionsDiagnostic(t.Context(), scope, 24, 10)
			if err == nil || !reflect.DeepEqual(got, ActionsDiagnostic{}) || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatal("failed/forged facts survived", got, err)
			}
			refreshStop(t, cancel, done)
		})
	}
}
func TestActionsDiagnosticHTTPUsesReadGrantRejectsForeignScopeAndStrictInputs(t *testing.T) {
	f, _, reader := diagnosticSetup(t)
	cancel, done := refreshStart(t, f, reader)
	defer refreshStop(t, cancel, done)
	mux := http.NewServeMux()
	f.e.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	for _, body := range []string{`{}`, `null`, `{"age_hours":0,"limit":10}`, `{"age_hours":24,"limit":21}`, `{"age_hours":24,"limit":10,"repository":"foreign/repo"}`, `{"age_hours":24,"age_hours":25,"limit":10}`, `{"age_hours":null,"limit":10}`} {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/projects/"+attemptProject+"/reviews/actions-liveness/observe", strings.NewReader(body))
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
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/projects/"+otherAttemptProject+"/reviews/actions-liveness/observe", strings.NewReader(`{"age_hours":24,"limit":10}`))
	request.Header.Set("Authorization", "Bearer "+f.reader)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || reader.scans.Load() != 0 {
		t.Fatal("foreign project reached provider", response.StatusCode, reader.scans.Load())
	}
	request, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/projects/"+attemptProject+"/reviews/actions-liveness/observe", strings.NewReader(`{"age_hours":24,"limit":10}`))
	request.Header.Set("Authorization", "Bearer "+f.reader)
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got ActionsDiagnostic
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&got) != nil || got.ProjectID != attemptProject || reader.scans.Load() != 1 {
		t.Fatal("bound read grant failed", response.StatusCode, got, reader.scans.Load())
	}
}

func TestActionsDiagnosticProjectionByteBoundAndStableOrdering(t *testing.T) {
	now := time.Now().UTC()
	request := &actionsDiagnosticRequest{started: now.Add(-time.Second), binding: actionsDiagnosticBinding{project: strings.Repeat("p", 128), repository: strings.Repeat("r", 100) + "/" + strings.Repeat("n", 100), repositoryID: 801}, age: 24, limit: 20}
	facts := diagnosticFacts(request.binding.repository, 801, 20)
	for i := range facts.Runs {
		facts.Runs[i].Event = strings.Repeat("e", 64)
		facts.Runs[i].UpdatedAt = now.Add(-48 * time.Hour)
		facts.Runs[i].ID = int64(9223372036854775807 - i)
		facts.Runs[i].WorkflowID = 9223372036854775807
		facts.Runs[i].CheckSuiteID = 9223372036854775807
		facts.Runs[i].RunNumber = int64(9223372036854775807 - i)
		facts.Runs[i].RunAttempt = 9223372036854775807
	}
	got, err := projectActionsDiagnostic(request, facts)
	raw, _ := json.Marshal(got)
	if err != nil || len(raw)+1 > actionsDiagnosticBytes || len(got.Runs) == 0 || len(got.Runs) >= 20 || got.Omitted != 20-len(got.Runs) || got.Scanned != 20 || got.Aged != 20 {
		t.Fatal("byte bound did not retain truthful compact facts", len(raw), got, err)
	}
	for i, row := range got.Runs {
		if row.ID != int64(9223372036854775788+i) {
			t.Fatal("equal-time rows did not use stable identity ordering", got.Runs)
		}
	}
}
