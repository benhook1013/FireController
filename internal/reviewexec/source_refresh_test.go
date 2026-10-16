package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type refreshTestReader struct {
	repository func(context.Context, int64) (reviewprovider.Repository, error)
	pull       func(context.Context, string, int64) (reviewprovider.Pull, error)
	calls      atomic.Int64
}

func (r *refreshTestReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	r.calls.Add(1)
	if r.repository != nil {
		return r.repository(ctx, id)
	}
	return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
}
func (r *refreshTestReader) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	r.calls.Add(1)
	if r.pull != nil {
		return r.pull(ctx, repository, number)
	}
	return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
}
func refreshRequest() SourceRefreshRequest {
	return SourceRefreshRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}
}
func refreshEnqueue(t *testing.T, f *attemptFixture, key string, r SourceRefreshRequest) Receipt {
	t.Helper()
	raw, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), key, r)
	if err != nil {
		t.Fatal(err)
	}
	var result Receipt
	if json.Unmarshal(raw, &result) != nil || !validRefreshID(result.ID) || result.State != "queued" || result.Revision != 1 {
		t.Fatal("invalid receipt", string(raw))
	}
	return result
}
func refreshRead(t *testing.T, f *attemptFixture, id string) SourceRefreshOperation {
	t.Helper()
	value, err := f.e.ReadSourceRefresh(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func refreshWait(t *testing.T, f *attemptFixture, id, state string) SourceRefreshOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		value := refreshRead(t, f, id)
		if value.State == state {
			return value
		}
		if value.State == "held" || value.State == "completed" {
			t.Fatalf("refresh state %s/%s instead of %s", value.State, value.Reason, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("refresh did not reach bounded state", state)
	return SourceRefreshOperation{}
}
func refreshStop(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err, ok := <-done:
		if !ok || err != nil {
			t.Fatal("source worker failed", err)
		}
		if _, ok = <-done; ok {
			t.Fatal("terminal channel not closed")
		}
	case <-time.After(time.Second):
		t.Fatal("source worker did not stop")
	}
}
func refreshStart(t *testing.T, f *attemptFixture, r TargetReader) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartSourceRefresh(ctx, map[string]TargetReader{"test-capability": r})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("source worker cleanup did not finish")
		}
	})
	return cancel, done
}

func TestSourceRefreshReplayCoalescingCurrentAuthAndPause(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, true)
	scope := f.scope(t, attemptProject, f.writer, false)
	r := refreshRequest()
	var wg sync.WaitGroup
	results := make(chan json.RawMessage, 8)
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			raw, err := f.e.EnqueueSourceRefresh(t.Context(), scope, fmt.Sprintf("coalesce-%d", i), r)
			results <- raw
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first json.RawMessage
	var receipt Receipt
	for raw := range results {
		if first == nil {
			first = raw
			if json.Unmarshal(raw, &receipt) != nil {
				t.Fatal("receipt")
			}
		} else if !bytes.Equal(first, raw) {
			t.Fatal("same target/actor did not coalesce")
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.source_refresh_requested'`) != 1 {
		t.Fatal("coalescing duplicated work/events")
	}
	// Original actor and worker are separate authorization contexts.
	_, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.admin, true), "different-actor", r)
	if err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `INSERT INTO workers(id,project_id,name) VALUES('refresh-worker',?,'Builder')`, attemptProject)
	workerScope := scope
	workerScope.WorkerID = "refresh-worker"
	_, err = f.e.EnqueueSourceRefresh(t.Context(), workerScope, "different-worker", r)
	if err != nil {
		t.Fatal(err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 3 {
		t.Fatal("different authority coalesced")
	}
	reader := &refreshTestReader{}
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.Observation == nil || got.ConfigurationRevision != 1 || got.Observation.HeadSHA != r.HeadSHA {
		t.Fatal("paused execution prevented source read", got)
	}
	// Replay is the original queued receipt after completion and config movement.
	f.configure(t, attemptProject, "test-capability", 1, false)
	raw, err := f.e.EnqueueSourceRefresh(t.Context(), scope, "coalesce-0", r)
	if err != nil || !bytes.Equal(raw, first) {
		t.Fatal("exact replay rebound operation", err)
	}
	changed := r
	changed.HeadSHA = strings.Repeat("c", 40)
	_, err = f.e.EnqueueSourceRefresh(t.Context(), scope, "coalesce-0", changed)
	attemptCode(t, err, "idempotency_conflict")
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	_, err = f.e.EnqueueSourceRefresh(t.Context(), scope, "coalesce-0", r)
	attemptCode(t, err, "unauthorized")
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("source refresh created review execution")
	}
}

func TestSourceRefreshProviderWaitLeavesWriterFreeAndRechecksBinding(t *testing.T) {
	for _, change := range []string{"configuration", "repository", "revoked", "grant", "capability", "pause-only"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			receipt := refreshEnqueue(t, f, "read", refreshRequest())
			entered, release := make(chan struct{}), make(chan struct{})
			reader := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return reviewprovider.Repository{}, ctx.Err()
				}
				return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
			}}
			cancel, done := refreshStart(t, f, reader)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("provider not reached")
			}
			// A real ordinary transaction must finish while the provider remains blocked.
			writeCtx, stop := context.WithTimeout(t.Context(), time.Second)
			err := f.store.BackgroundWrite(writeCtx, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE projects SET name='refreshed local name' WHERE id=?`, attemptProject)
				return err
			})
			stop()
			if err != nil {
				t.Fatal("provider wait occupied writer", err)
			}
			switch change {
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			case "pause-only":
				f.configure(t, attemptProject, "test-capability", 1, true)
			case "repository":
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
			case "revoked":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "grant":
				admissionSQL(t, f, `UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`)
			case "capability":
				// Capabilities are immutable per Engine; startup changes are tested separately.
				f.configure(t, attemptProject, "other-account", 1, false)
			}
			close(release)
			want := "held"
			if change == "pause-only" {
				want = "completed"
			}
			got := refreshWait(t, f, receipt.ID, want)
			refreshStop(t, cancel, done)
			if want == "held" && (got.Observation != nil || got.Reason == "") {
				t.Fatal("stale binding published observation", got)
			}
			if want == "completed" {
				if got.ConfigurationRevision != 1 {
					t.Fatal("original request revision rewritten")
				}
				assessment, err := f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 2)
				if err != nil || assessment.ConfigurationRevision != 2 || assessment.Reason != "" {
					t.Fatal("pause-only revision invalidated observation", assessment, err)
				}
			}
		})
	}
}

func TestSourceRefreshCancellationRestartAndProviderFailureRetainsPriorObservation(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	first := refreshEnqueue(t, f, "first", refreshRequest())
	reader := &refreshTestReader{}
	cancel, done := refreshStart(t, f, reader)
	previous := refreshWait(t, f, first.ID, "completed")
	refreshStop(t, cancel, done)
	next := refreshEnqueue(t, f, "interrupted", refreshRequest())
	entered := make(chan struct{})
	closed := make(chan struct{})
	blocked := &refreshTestReader{repository: func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		close(entered)
		<-ctx.Done()
		close(closed)
		return reviewprovider.Repository{}, ctx.Err()
	}}
	cancel, done = refreshStart(t, f, blocked)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider not reached")
	}
	refreshStop(t, cancel, done)
	<-closed
	if got := refreshRead(t, f, next.ID); got.State != "running" || got.Observation != nil {
		t.Fatal("cancelled source read invented terminal result", got)
	}
	// Actual database close/reopen, not just a new context on an existing engine.
	var database string
	if err := f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	if err = EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	moved := &refreshTestReader{pull: func(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	cancel, done = refreshStart(t, f, moved)
	got := refreshWait(t, f, next.ID, "held")
	refreshStop(t, cancel, done)
	if got.Reason != "target_changed" || got.Revision != 5 {
		t.Fatal("interrupted read not requeued exactly before failed retry", got)
	}
	assessment, err := f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 2)
	if err != nil || assessment.RefreshID != first.ID || !reflect.DeepEqual(assessment.Target, *previous.Observation) {
		t.Fatal("failed later refresh replaced previous complete observation", assessment, err)
	}
	replay, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), "interrupted", refreshRequest())
	if err != nil || !strings.Contains(string(replay), next.ID) {
		t.Fatal("restart lost immutable receipt", err)
	}
}

func TestSourceRefreshCurrentAssessmentUsesNativeCoverageAndCurrentPolicy(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, id, 0)
	receipt := refreshEnqueue(t, f, "source", refreshRequest())
	reader := &refreshTestReader{}
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	before := adjudicationState(t, f, a) + policyState(t, f)
	assessment, err := f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || assessment.State != "held" || assessment.Source != "observed_comparison" || len(assessment.Channels) != 2 || !assessment.Channels[0].Covered || assessment.Channels[1].Covered {
		t.Fatal("source assessment mixed channel/native credit", assessment, err)
	}
	if before != adjudicationState(t, f, a)+policyState(t, f) {
		t.Fatal("assessment mutated journal")
	}
	// Imported fully closed hosted projection must still contribute zero credit.
	if err = reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reviews.Register(mux, f.store)
	f.handler = mux
	pull := projectionPull(2)
	pull.Runs = []reviews.RunInput{{SourceRunID: "historical", Channel: "hosted", HeadSHA: a.Anchor.CandidateSHA, Reviewer: "Legacy", Scope: "historical", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: stamp(), FinishedAt: stamp(), CoverageLimits: []string{}, Findings: []reviews.Finding{}}}
	response := f.request(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "source-import", encoded(reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "historical-import", ObservedAt: stamp(), Queue: []reviews.PullInput{pull}, Retired: []reviews.RetiredPull{}}))
	if response.Code != http.StatusCreated {
		t.Fatal("import fixture", response.Code, response.Body.String())
	}
	assessment, err = f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || assessment.State != "held" || assessment.Channels[1].Covered {
		t.Fatal("imported history credited hosted", assessment, err)
	}
	assessmentPolicy(t, f, 1, []string{"cli"}, []string{"cli"})
	assessment, err = f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || assessment.State != "sufficient" || assessment.PolicyRevision != 2 || assessment.Channels[0].Pending {
		t.Fatal("current paused policy erased credit", assessment, err)
	}
	f.configure(t, attemptProject, "test-capability", 1, true)
	assessment, err = f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || assessment.State != "sufficient" || assessment.ConfigurationRevision != 2 {
		t.Fatal("pause-only config erased credit", assessment, err)
	}
	f.configure(t, attemptProject, "other-account", 2, true)
	assessment, err = f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || assessment.State != "held" || assessment.Reason != "source_configuration_changed" || len(assessment.Channels) != 0 || assessment.ConfigurationRevision != 3 {
		t.Fatal("changed config credited stale observation", assessment, err)
	}
}

func TestSourceRefreshCapacityIsolationAndStartupFailures(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	for i := range 16 {
		r := refreshRequest()
		r.HeadSHA = fmt.Sprintf("%040x", i)
		refreshEnqueue(t, f, fmt.Sprintf("capacity-%d", i), r)
	}
	r := refreshRequest()
	_, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), "over-cap", r)
	attemptCode(t, err, "overloaded")
	raw, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, otherAttemptProject, f.other, false), "other-project", r)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	_ = json.Unmarshal(raw, &receipt)
	_, err = f.e.ReadSourceRefresh(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), receipt.ID)
	attemptCode(t, err, "not_found")
	if done, err := f.e.StartSourceRefresh(t.Context(), nil); done != nil || err != nil {
		t.Fatal("empty catalog starts a worker", err)
	}
	if _, err = f.e.StartSourceRefresh(nil, map[string]TargetReader{"test-capability": &refreshTestReader{}}); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err = f.e.StartSourceRefresh(t.Context(), map[string]TargetReader{"unknown": &refreshTestReader{}}); err == nil {
		t.Fatal("unknown reader accepted")
	}
	var nilReader *refreshTestReader
	if _, err = f.e.StartSourceRefresh(t.Context(), map[string]TargetReader{"test-capability": nilReader}); err == nil {
		t.Fatal("typed nil reader accepted")
	}
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if _, err = f.e.StartSourceRefresh(cancelled, map[string]TargetReader{"test-capability": &refreshTestReader{}}); err == nil {
		t.Fatal("startup cancellation hidden")
	}
	if f.e.sourceRunning.Load() {
		t.Fatal("startup failure leaked ownership")
	}
	// Do not start providers for another actor after its current grant disappears.
	admissionSQL(t, f, `UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`)
	reader := &refreshTestReader{}
	cancel, done := refreshStart(t, f, reader)
	if _, err = f.e.StartSourceRefresh(t.Context(), map[string]TargetReader{"test-capability": reader}); err == nil {
		t.Fatal("duplicate worker accepted")
	}
	// Project fairness permits the independent read before denied work drains.
	// Wait for both outcomes before cancellation; neither implies the other.
	scopeOther := evidenceReadScope(t, f, otherAttemptProject, f.other)
	deadline := time.Now().Add(3 * time.Second)
	drained := false
	for time.Now().Before(deadline) {
		got, err := f.e.ReadSourceRefresh(t.Context(), scopeOther, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == "held" && attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE project_id=? AND state='held' AND reason='authorization_changed'`, attemptProject) == 16 {
			drained = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !drained {
		t.Fatal("independent read and all denied operations did not reach terminal states")
	}
	refreshStop(t, cancel, done)
	if reader.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE project_id=? AND state='held' AND reason='authorization_changed'`, attemptProject) != 16 {
		t.Fatal("queued revocation reached provider or blocked FIFO drain", reader.calls.Load())
	}
}

func TestSourceRefreshMalformedRetainedRowsFailClosed(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "corrupt", refreshRequest())
	admissionSQL(t, f, `UPDATE review_source_refreshes SET observation_json='{}',state='completed' WHERE id=?`, receipt.ID)
	_, err := f.e.ReadSourceRefresh(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), receipt.ID)
	attemptCode(t, err, "storage_unavailable")
	_, err = f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 2)
	attemptCode(t, err, "storage_unavailable")
}

func TestSourceRefreshGlobalCapacityAndBoundedIndexedQueue(t *testing.T) {
	f := newAttemptFixture(t)
	for projectIndex := range 5 {
		project := fmt.Sprintf("refresh-project-%d", projectIndex)
		admissionSQL(t, f, `INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)`, project, project, 900+projectIndex, "owner/repo")
		f.configure(t, project, "test-capability", 0, false)
		for index := range 16 {
			r := refreshRequest()
			r.HeadSHA = fmt.Sprintf("%040x", index)
			_, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, project, f.admin, true), fmt.Sprintf("global-%d-%d", projectIndex, index), r)
			if projectIndex == 4 {
				attemptCode(t, err, "overloaded")
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes`) != 64 {
		t.Fatal("global cap not exact")
	}
	// Explain the actual FIFO and completed-target reads; history cannot introduce
	// an unindexed body scan or temporary ordering table into these bounded reads.
	for _, query := range []string{
		`EXPLAIN QUERY PLAN SELECT project_id,id FROM review_source_refreshes WHERE state='queued' ORDER BY created_at,id LIMIT 1`,
		`EXPLAIN QUERY PLAN SELECT id FROM review_source_refreshes WHERE project_id='refresh-project-0' AND pull_number=2 AND state='completed' ORDER BY created_at DESC,id DESC LIMIT 1`,
	} {
		rows, err := f.store.DB().Query(query)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), "INDEX review_source_refresh_") || strings.Contains(plan.String(), "TEMP B-TREE") {
			t.Fatal("source history query lost ordered index", plan.String())
		}
	}
}
func TestSourceRefreshRetainedCapabilityChangeAndMissingPolicy(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, true)
	receipt := refreshEnqueue(t, f, "old-capability", refreshRequest())
	capabilities := append([]Capability(nil), attemptCapabilities...)
	capabilities[0].Fingerprint = strings.Repeat("9", 64)
	var err error
	f.e, err = New(f.store, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	reader := &refreshTestReader{}
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "held")
	refreshStop(t, cancel, done)
	if got.Reason != "source_configuration_changed" || reader.calls.Load() != 0 {
		t.Fatal("queued request silently rebound capability", got, reader.calls.Load())
	}
	next := refreshEnqueue(t, f, "new-capability", refreshRequest())
	cancel, done = refreshStart(t, f, reader)
	refreshWait(t, f, next.ID, "completed")
	refreshStop(t, cancel, done)
	admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
	assessment, err := f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 2)
	if err != nil || assessment.State != "unconfigured" || assessment.PolicyRevision != 0 || len(assessment.Channels) != 0 {
		t.Fatal("missing requirements passed vacuously", assessment, err)
	}
	f.configure(t, attemptProject, "other-account", 1, false)
	assessment, err = f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 2)
	if err != nil || assessment.State != "held" || assessment.Reason != "source_configuration_changed" || assessment.PolicyRevision != 0 || assessment.ConfigurationRevision != 2 {
		t.Fatal("stale binding precedence lost", assessment, err)
	}
}
func TestSourceRefreshFIFOAndRelevantStackParentOnly(t *testing.T) {
	f := newAttemptFixture(t)
	revision := int64(0)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack-source", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{1, 2, 4, 8}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
	r := refreshRequest()
	r.PullNumber = 4
	receipt := refreshEnqueue(t, f, "selected-stack", r)
	var mu sync.Mutex
	var numbers []int64
	reader := &refreshTestReader{pull: func(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
		mu.Lock()
		numbers = append(numbers, number)
		mu.Unlock()
		head := strings.Repeat("a", 40)
		if number == 2 {
			head = strings.Repeat("b", 40)
		}
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: head, BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	cancel, done := refreshStart(t, f, reader)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.Observation.ParentPullNumber != 2 || !reflect.DeepEqual(numbers, []int64{4, 2}) || reader.calls.Load() != 3 {
		t.Fatal("source refresh read unrelated queue entries or lost effective parent", numbers, got)
	}
}

func TestSourceRefreshEmptyQueueNeverEntersHeldWriter(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &refreshTestReader{}
	cancel, done := refreshStart(t, f, reader)
	entered, release := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET name='writer held' WHERE id=?`, attemptProject); err != nil {
				return err
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fixture writer not held")
	}
	defer func() {
		close(release)
		select {
		case err := <-writeDone:
			if err != nil {
				t.Error("fixture writer failed", err)
			}
		case <-time.After(time.Second):
			t.Error("fixture writer did not stop")
		}
	}()
	// The old idle BackgroundWrite would fail this derived deadline while the
	// canonical writer is occupied; an empty indexed snapshot completes directly.
	localCtx, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	worked, err := f.e.refreshNext(localCtx, map[string]TargetReader{"test-capability": reader})
	stop()
	if err != nil || worked {
		t.Fatal("empty source queue contended for writer admission", worked, err)
	}
	time.Sleep(550 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatal("idle source worker failed behind writer", err)
	default:
	}
	refreshStop(t, cancel, done)
	if reader.calls.Load() != 0 {
		t.Fatal("idle source worker called provider")
	}
	// Releasing is deferred so the writer remains occupied throughout shutdown.
}

func TestSourceRefreshMovedHeadStillReportsOlderPendingNativeEvidence(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 1, false)
	adjudicationComplete(t, f, a)
	request := refreshRequest()
	request.HeadSHA = strings.Repeat("c", 40)
	receipt := refreshEnqueue(t, f, "moved-head", request)
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: request.HeadSHA, BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	value, err := f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || value.State != "held" || value.Target.HeadSHA != request.HeadSHA || !value.Channels[0].Pending || value.Channels[0].Covered {
		t.Fatal("new observation bypassed older native obligations", value, err)
	}
	adjudicationDecide(t, f, id, "older-rejected", 0, 0, "rejected")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "older-finalized", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, id, "finalized", 2)
	value, err = f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || value.State != "held" || value.Channels[0].Pending || value.Channels[0].Covered {
		t.Fatal("closed old comparison became current credit", value, err)
	}
}
func TestSourceRefreshStorageFailureIsVisibleAndStartupFailureReleasesOwnership(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		f := newAttemptFixture(t)
		f.configure(t, attemptProject, "test-capability", 0, false)
		receipt := refreshEnqueue(t, f, "interrupted", refreshRequest())
		admissionSQL(t, f, `UPDATE review_source_refreshes SET state='running',revision=2 WHERE id=?`, receipt.ID)
		admissionSQL(t, f, `CREATE TRIGGER source_recovery_fault BEFORE UPDATE ON review_source_refreshes BEGIN SELECT RAISE(ABORT,'synthetic recovery fault'); END`)
		reader := &refreshTestReader{}
		done, err := f.e.StartSourceRefresh(t.Context(), map[string]TargetReader{"test-capability": reader})
		if err == nil || done != nil || f.e.sourceRunning.Load() || reader.calls.Load() != 0 {
			t.Fatal("failed recovery leaked worker/provider/ownership", err)
		}
		admissionSQL(t, f, `DROP TRIGGER source_recovery_fault`)
		cancel, done := refreshStart(t, f, reader)
		got := refreshWait(t, f, receipt.ID, "completed")
		refreshStop(t, cancel, done)
		if got.Revision != 5 || reader.calls.Load() != 2 {
			t.Fatal("startup recovery did not retain/recover one read", got, reader.calls.Load())
		}
	})
	t.Run("worker", func(t *testing.T) {
		f := newAttemptFixture(t)
		f.configure(t, attemptProject, "test-capability", 0, false)
		reader := &refreshTestReader{}
		cancel, done := refreshStart(t, f, reader)
		defer cancel()
		admissionSQL(t, f, `DROP TABLE review_source_refreshes`)
		f.e.sourceWake <- struct{}{}
		select {
		case err := <-done:
			attemptCode(t, err, "source_refresh_persistence_failed")
		case <-time.After(time.Second):
			t.Fatal("storage failure silently stopped/stalled source worker")
		}
		if f.e.sourceRunning.Load() || reader.calls.Load() != 0 {
			t.Fatal("storage failure leaked worker/provider")
		}
	})
}
func TestSourceRefreshFixedTimestampOrderingAndLatestCompletedSelection(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	first := refreshEnqueue(t, f, "first-order", refreshRequest())
	r := refreshRequest()
	r.PullNumber = 4
	second := refreshEnqueue(t, f, "second-order", r)
	for _, id := range []string{first.ID, second.ID} {
		value := refreshRead(t, f, id)
		for _, stamp := range []string{value.CreatedAt, value.UpdatedAt} {
			parsed, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil || stamp != parsed.UTC().Format("2006-01-02T15:04:05.000000000Z") {
				t.Fatal("source ordering timestamp has variable width", stamp)
			}
		}
	}
	admissionSQL(t, f, `UPDATE review_source_refreshes SET created_at='2026-01-01T00:00:00.100000000Z' WHERE id=?`, first.ID)
	admissionSQL(t, f, `UPDATE review_source_refreshes SET created_at='2026-01-01T00:00:00.110000000Z' WHERE id=?`, second.ID)
	var mu sync.Mutex
	var order []int64
	reader := &refreshTestReader{pull: func(ctx context.Context, repo string, number int64) (reviewprovider.Pull, error) {
		mu.Lock()
		order = append(order, number)
		mu.Unlock()
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, second.ID, "completed")
	refreshStop(t, cancel, done)
	if !reflect.DeepEqual(order, []int64{2, 4}) {
		t.Fatal("source FIFO is not chronological", order)
	}
	// A later completed same-PR observation must win the stored assessment lookup.
	third := refreshEnqueue(t, f, "latest-order", refreshRequest())
	cancel, done = refreshStart(t, f, reader)
	refreshWait(t, f, third.ID, "completed")
	refreshStop(t, cancel, done)
	admissionSQL(t, f, `UPDATE review_source_refreshes SET created_at='2026-01-01T00:00:00.110000000Z' WHERE id=?`, third.ID)
	value, err := f.e.ReadSourcePolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), 2)
	if err != nil || value.RefreshID != third.ID {
		t.Fatal("latest source observation misordered fractional timestamps", value, err)
	}
}
