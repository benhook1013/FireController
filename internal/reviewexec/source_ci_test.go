package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type sourceCITestReader struct {
	refreshTestReader
	ci      func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error)
	ciCalls atomic.Int64
}

func (r *sourceCITestReader) ObserveRequiredCI(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
	r.ciCalls.Add(1)
	if r.ci != nil {
		return r.ci(ctx, target)
	}
	return sourceCITestObservation(target, "satisfied"), nil
}
func sourceCITestObservation(target reviewprovider.CITarget, state string) reviewprovider.CIObservation {
	now := time.Now().UTC()
	o := reviewprovider.CIObservation{Target: target, StartedAt: now, ObservedAt: now, State: state, MergeState: "conflicting", Authority: reviewprovider.RequiredCheckAuthority{State: "complete", Repository: target.Repository, Branch: target.BaseRef, ObservedAt: now, Checks: []reviewprovider.RequiredCheck{{Context: "build", AppID: 0}}}, Candidate: &reviewprovider.CICandidate{State: "complete", RepositoryID: target.RepositoryID, Repository: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, MergeState: "conflicting", ObservedAt: now}}
	if state == "not_required" {
		o.Authority.Checks = []reviewprovider.RequiredCheck{}
		o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, reviewprovider.CIResultInventory{Repository: target.Repository, HeadSHA: target.HeadSHA}, reviewprovider.ActionsHeadInventory{}, nil)
		return o
	}
	o.Selection, o.SelectedCommitSHA = "head", target.HeadSHA
	status := "success"
	if state == "failed" {
		status = "failure"
	}
	if state == "pending" {
		status = "pending"
	}
	o.Results = &reviewprovider.CIResultInventory{Repository: target.Repository, HeadSHA: target.HeadSHA, ObservedAt: now, Checks: []reviewprovider.CICheckRun{}, Statuses: []reviewprovider.CICommitStatus{{ID: 991, Context: "build", State: status, CreatedAt: now, UpdatedAt: now}}}
	o.Actions = &reviewprovider.ActionsHeadInventory{Repository: target.Repository, RepositoryID: target.RepositoryID, HeadSHA: target.HeadSHA, ObservedAt: now, Runs: []reviewprovider.ActionsHeadRun{}}
	o.Attempts = []reviewprovider.ActionsAttempt{}
	o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
	o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	return o
}
func sourceCIRequest() SourceRefreshRequest { r := refreshRequest(); r.IncludeCI = true; return r }
func sourceCIScope(t *testing.T, f *attemptFixture) core.Scope {
	return evidenceReadScope(t, f, attemptProject, f.reader)
}
func readSourceCI(t *testing.T, f *attemptFixture) SourceCIAssessment {
	t.Helper()
	v, err := f.e.ReadSourceCIAssessment(t.Context(), sourceCIScope(t, f), 2)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sourceCITestState(t *testing.T, f *attemptFixture) string {
	t.Helper()
	var rows string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(id||state||revision||updated_at||observation_json||ci_observation_json,''),'') FROM review_source_refreshes`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return policyState(t, f) + "/" + rows
}

func TestSourceCIOptionCoalescingFingerprintAndRestartMigration(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	plain := refreshEnqueue(t, f, "plain", refreshRequest())
	ci := refreshEnqueue(t, f, "ci", sourceCIRequest())
	again := refreshEnqueue(t, f, "ci-other-key", sourceCIRequest())
	if plain.ID == ci.ID || ci != again {
		t.Fatal("fact sets coalesced incorrectly", plain, ci, again)
	}
	body, _ := json.Marshal(refreshRequest())
	if strings.Contains(string(body), "include_ci") {
		t.Fatal("false option changed retained request fingerprint", string(body))
	}
	_, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), "plain", sourceCIRequest())
	attemptCode(t, err, "idempotency_conflict")
	// Reconstruct the actual prior v1 table shape with retained queued facts. V2
	// must preserve those rows/receipts while defaulting their new flag to false.
	admissionSQL(t, f, `DELETE FROM review_source_refreshes WHERE id=?`, ci.ID)
	admissionSQL(t, f, `DROP INDEX review_source_refresh_coalesce`)
	admissionSQL(t, f, `DROP INDEX review_source_refresh_ci_latest`)
	admissionSQL(t, f, `DROP INDEX review_source_refresh_readiness`)
	admissionSQL(t, f, `ALTER TABLE review_source_refreshes DROP COLUMN include_readiness`)
	admissionSQL(t, f, `ALTER TABLE review_source_refreshes DROP COLUMN review_state_json`)
	admissionSQL(t, f, `DROP INDEX review_source_refresh_ci_target`)
	admissionSQL(t, f, `ALTER TABLE review_source_refreshes DROP COLUMN include_ci`)
	admissionSQL(t, f, `ALTER TABLE review_source_refreshes DROP COLUMN ci_observation_json`)
	admissionSQL(t, f, `CREATE INDEX review_source_refresh_coalesce ON review_source_refreshes(project_id,repository_id,pull_number,head_sha,configuration_revision,capability_fingerprint,actor_id,worker_id,state) WHERE state IN ('queued','running')`)
	admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_source_refresh'`)
	if err = EnsureSourceRefreshSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	got := refreshRead(t, f, plain.ID)
	if got.IncludeCI || got.CI != nil || got.State != "queued" {
		t.Fatal("migration invented CI", got)
	}
	raw, err := f.e.EnqueueSourceRefresh(t.Context(), f.scope(t, attemptProject, f.writer, false), "plain", refreshRequest())
	if err != nil || !strings.Contains(string(raw), plain.ID) {
		t.Fatal("migration lost original key", string(raw), err)
	}
	var plan string
	rows, err := f.store.DB().Query(`EXPLAIN QUERY PLAN SELECT id FROM review_source_refreshes WHERE project_id=? AND pull_number=? AND state='completed' AND include_ci=1 ORDER BY created_at DESC,id DESC LIMIT 1`, attemptProject, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if !strings.Contains(plan, "review_source_refresh_ci_target") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("CI read not indexed", plan)
	}
}

func TestSourceCICompletedPrivateInventoryCompactReadAndNoProviderReplay(t *testing.T) {
	for _, state := range []string{"satisfied", "failed", "pending", "not_required"} {
		t.Run(state, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, true)
			reader := &sourceCITestReader{ci: func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return sourceCITestObservation(target, state), nil
			}}
			receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			got := refreshWait(t, f, receipt.ID, "completed")
			refreshStop(t, cancel, done)
			if !got.IncludeCI || got.Observation == nil || got.CI == nil || got.CI.State != state || reader.ciCalls.Load() != 1 || reader.calls.Load() != 2 {
				t.Fatal("CI/source collection mismatch", got, reader.ciCalls.Load(), reader.calls.Load())
			}
			if state == "not_required" {
				if got.CI.Required != 0 || got.CI.Selection != "" {
					t.Fatal(got.CI)
				}
			} else if got.CI.Required != 1 {
				t.Fatal(got.CI)
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), `"status_id":`) || strings.Contains(string(raw), `"authority":`) || len(raw) > 4096 {
				t.Fatal("ordinary operation exposed inventories", string(raw))
			}
			var private string
			if err := f.store.DB().QueryRow(`SELECT ci_observation_json FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&private); err != nil || !strings.Contains(private, "authority") {
				t.Fatal("full CI observation not retained", err)
			}
			before := sourceCITestState(t, f)
			calls := reader.calls.Load()
			ciCalls := reader.ciCalls.Load()
			assessment := readSourceCI(t, f)
			if assessment.Source != "observed_ci" || assessment.State != state || assessment.RefreshID != receipt.ID || assessment.ConfigurationRevision != 1 || assessment.SourceObservedAt != got.Observation.ObservedAt {
				t.Fatal(assessment)
			}
			if _, err := f.e.ReadSourceRefresh(t.Context(), sourceCIScope(t, f), receipt.ID); err != nil {
				t.Fatal(err)
			}
			again := refreshEnqueue(t, f, "ci", sourceCIRequest())
			if again != receipt || reader.calls.Load() != calls || reader.ciCalls.Load() != ciCalls || !reflect.DeepEqual(before, sourceCITestState(t, f)) {
				t.Fatal("reads/replay repeated provider or writes")
			}
			_, err := f.e.ReadSourceCIAssessment(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), 2)
			attemptCode(t, err, "reviews_ci_unavailable")
		})
	}
}

func TestSourceCIFailuresPreserveSourceWithoutCredit(t *testing.T) {
	cases := map[string]func(*sourceCITestReader){
		"provider": func(r *sourceCITestReader) {
			r.ci = func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return reviewprovider.CIObservation{}, errors.New("PRIVATE provider diagnostic")
			}
		},
		"wrongtarget": func(r *sourceCITestReader) {
			r.ci = func(ctx context.Context, t reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(t, "satisfied")
				o.Target.HeadSHA = strings.Repeat("c", 40)
				return o, nil
			}
		},
		"wrongassessment": func(r *sourceCITestReader) {
			r.ci = func(ctx context.Context, t reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(t, "satisfied")
				o.Assessment.Checks[0].StatusID++
				return o, nil
			}
		},
		"time": func(r *sourceCITestReader) {
			r.ci = func(ctx context.Context, t reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(t, "satisfied")
				o.ObservedAt = o.StartedAt.Add(-time.Second)
				return o, nil
			}
		},
		"capacity": func(r *sourceCITestReader) {
			r.ci = func(ctx context.Context, t reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(t, "satisfied")
				o.Authority.Reason = strings.Repeat("x", sourceCIMaxBytes)
				return o, nil
			}
		},
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := &sourceCITestReader{}
			configure(r)
			receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
			cancel, done := refreshStart(t, f, r)
			got := refreshWait(t, f, receipt.ID, "completed")
			refreshStop(t, cancel, done)
			if got.Observation == nil || got.CI == nil || got.CI.State != "unproven" || got.CI.Required != 0 || got.CI.Reason == "" {
				t.Fatal(got)
			}
			wire, _ := json.Marshal(got)
			if strings.Contains(string(wire), "PRIVATE") {
				t.Fatal("private diagnostics leaked")
			}
			var body string
			if err := f.store.DB().QueryRow(`SELECT ci_observation_json FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&body); err != nil || len(body) > 4096 || strings.Contains(body, "build") {
				t.Fatal("partial/oversized inventories retained", err)
			}
		})
	}
	t.Run("absentreader", func(t *testing.T) {
		f := newAttemptFixture(t)
		f.configure(t, attemptProject, "test-capability", 0, false)
		receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
		cancel, done := refreshStart(t, f, &refreshTestReader{})
		got := refreshWait(t, f, receipt.ID, "completed")
		refreshStop(t, cancel, done)
		if got.CI == nil || got.CI.Reason != "ci_reader_unavailable" {
			t.Fatal(got)
		}
	})
	t.Run("sourcefailure", func(t *testing.T) {
		f := newAttemptFixture(t)
		f.configure(t, attemptProject, "test-capability", 0, false)
		r := &sourceCITestReader{}
		r.pull = func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
			return reviewprovider.Pull{}, errors.New("source failed")
		}
		receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
		cancel, done := refreshStart(t, f, r)
		got := refreshWait(t, f, receipt.ID, "held")
		refreshStop(t, cancel, done)
		if got.CI != nil || got.Observation != nil || r.ciCalls.Load() != 0 {
			t.Fatal("CI called without source", got)
		}
	})
}

func TestSourceCIBlockedProviderDoesNotOwnWriterAndCurrentBindingsHold(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	entered, release := make(chan struct{}), make(chan struct{})
	r := &sourceCITestReader{ci: func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		close(entered)
		select {
		case <-release:
			return sourceCITestObservation(target, "satisfied"), nil
		case <-ctx.Done():
			return reviewprovider.CIObservation{}, ctx.Err()
		}
	}}
	receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
	cancel, done := refreshStart(t, f, r)
	defer cancel()
	<-entered
	// This serialized writer must remain available while CI is waiting.
	writeCtx, writeCancel := context.WithTimeout(t.Context(), time.Second)
	defer writeCancel()
	if err := f.store.BackgroundWrite(writeCtx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, attemptProject)
		return err
	}); err != nil {
		t.Fatal("CI waited under writer", err)
	}
	f.configure(t, attemptProject, "test-capability", 1, true)
	close(release)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.CI.State != "satisfied" {
		t.Fatal("pause-only invalidated observation", got)
	}
	if assessment := readSourceCI(t, f); assessment.State != "satisfied" || assessment.ConfigurationRevision != 2 {
		t.Fatal(assessment)
	}
	// A later base-only observation makes old CI inapplicable even on same head.
	next := refreshEnqueue(t, f, "newbase", SourceRefreshRequest{ExpectedConfigurationRevision: 2, PullNumber: 2, HeadSHA: refreshRequest().HeadSHA})
	moved := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{RepositoryID: 801, Number: n, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("c", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	cancel, done = refreshStart(t, f, moved)
	refreshWait(t, f, next.ID, "completed")
	refreshStop(t, cancel, done)
	if assessment := readSourceCI(t, f); assessment.State != "held" || assessment.Reason != "source_comparison_changed" || assessment.Satisfied != 1 {
		t.Fatal("base movement inherited credit", assessment)
	}
	f.configure(t, attemptProject, "other-account", 2, false)
	if assessment := readSourceCI(t, f); assessment.State != "held" || assessment.Reason != "source_configuration_changed" {
		t.Fatal(assessment)
	}
}

func TestSourceCICancelReopenRecoversReadOnlySameOperation(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
	entered := make(chan struct{})
	blocked := &sourceCITestReader{ci: func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		close(entered)
		<-ctx.Done()
		return reviewprovider.CIObservation{}, ctx.Err()
	}}
	cancel, done := refreshStart(t, f, blocked)
	<-entered
	refreshStop(t, cancel, done)
	if got := refreshRead(t, f, receipt.ID); got.State != "running" || !got.IncludeCI || got.CI != nil {
		t.Fatal(got)
	}
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
	r := &sourceCITestReader{}
	cancel, done = refreshStart(t, f, r)
	got := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if got.Revision != 5 || got.CI == nil || got.CI.State != "satisfied" || r.ciCalls.Load() != 1 {
		t.Fatal("restart didn't safely repeat same read", got)
	}
	var reservations int
	if err = store.DB().QueryRow(`SELECT count(*) FROM review_execution_reservations`).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatal("source refresh reserved external action", reservations, err)
	}
}

func TestSourceCICollectionBudgetsAndCancellationRetainRunning(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	var sourceDeadline, ciDeadline time.Time
	r := &sourceCITestReader{ci: func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		ciDeadline, _ = ctx.Deadline()
		return sourceCITestObservation(target, "not_required"), nil
	}}
	r.repository = func(ctx context.Context, id int64) (reviewprovider.Repository, error) {
		sourceDeadline, _ = ctx.Deadline()
		return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}
	receipt := refreshEnqueue(t, f, "budget", sourceCIRequest())
	started := time.Now()
	cancel, done := refreshStart(t, f, r)
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if sourceDeadline.Before(started.Add(14*time.Second)) || sourceDeadline.After(time.Now().Add(15*time.Second)) || ciDeadline.Sub(sourceDeadline) < 14*time.Second || ciDeadline.Sub(sourceDeadline) > 16*time.Second {
		t.Fatal("source and aggregate budgets changed", sourceDeadline, ciDeadline)
	}
}

func TestSourceCIChangedInFlightBindingDoesNotPublishCI(t *testing.T) {
	for _, change := range []string{"configuration", "authorization", "repository"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			entered, release := make(chan struct{}), make(chan struct{})
			r := &sourceCITestReader{ci: func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				close(entered)
				select {
				case <-release:
					return sourceCITestObservation(target, "satisfied"), nil
				case <-ctx.Done():
					return reviewprovider.CIObservation{}, ctx.Err()
				}
			}}
			receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
			cancel, done := refreshStart(t, f, r)
			defer cancel()
			<-entered
			switch change {
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			case "authorization":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "repository":
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
			}
			close(release)
			got := refreshWait(t, f, receipt.ID, "held")
			refreshStop(t, cancel, done)
			if got.Observation != nil || got.CI != nil || !got.IncludeCI {
				t.Fatal("changed binding retained successful source/CI", got)
			}
		})
	}
}

func TestSourceCIReadBindingAndStoredCorruptionFailClosed(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
	cancel, done := refreshStart(t, f, &sourceCITestReader{})
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	var original string
	if err := f.store.DB().QueryRow(`SELECT ci_observation_json FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for name, corrupt := range map[string]func(string) string{
		"unknown": func(s string) string { return strings.Replace(s, `"target":`, `"unknown":1,"target":`, 1) },
		"alias":   func(s string) string { return strings.Replace(s, `"target":`, `"Target":`, 1) },
		"duplicate": func(s string) string {
			return strings.Replace(s, `"state":"satisfied"`, `"state":"satisfied","state":"satisfied"`, 1)
		},
		"mismatchtarget": func(s string) string { return strings.Replace(s, `"repository_id":801`, `"repository_id":802`, 1) },
		"inventedcounts": func(s string) string { return strings.Replace(s, `"status_id":991`, `"status_id":992`, 1) },
		"missingtime":    func(s string) string { return strings.Replace(s, `"started_at":`, `"missing_started_at":`, 1) },
		"falsestate":     func(s string) string { return strings.Replace(s, `"state":"satisfied"`, `"state":"not_required"`, 1) },
		"null":           func(s string) string { return "null" },
	} {
		t.Run(name, func(t *testing.T) {
			body := corrupt(original)
			if body == original {
				t.Fatal("fixture did not change")
			}
			admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, body, receipt.ID)
			_, err := f.e.ReadSourceRefresh(t.Context(), sourceCIScope(t, f), receipt.ID)
			attemptCode(t, err, "storage_unavailable")
			_, err = f.e.ReadSourceCIAssessment(t.Context(), sourceCIScope(t, f), 2)
			attemptCode(t, err, "storage_unavailable")
			admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, original, receipt.ID)
		})
	}
	scope := sourceCIScope(t, f)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	_, err := f.e.ReadSourceCIAssessment(t.Context(), scope, 2)
	attemptCode(t, err, "unauthorized")
	// Another valid reader's project scope cannot inherit this CI observation.
	other := evidenceReadScope(t, f, otherAttemptProject, f.other)
	_, err = f.e.ReadSourceCIAssessment(t.Context(), other, 2)
	attemptCode(t, err, "reviews_ci_unavailable")
}

func TestSourceCILatestHeadAndRepositoryMovementHoldOldObservation(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	receipt := refreshEnqueue(t, f, "ci", sourceCIRequest())
	cancel, done := refreshStart(t, f, &sourceCITestReader{})
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	nextRequest := refreshRequest()
	nextRequest.HeadSHA = strings.Repeat("c", 40)
	next := refreshEnqueue(t, f, "newhead", nextRequest)
	moved := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{RepositoryID: 801, Number: n, State: "open", HeadSHA: nextRequest.HeadSHA, BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	cancel, done = refreshStart(t, f, moved)
	refreshWait(t, f, next.ID, "completed")
	refreshStop(t, cancel, done)
	got := readSourceCI(t, f)
	if got.State != "held" || got.Reason != "source_comparison_changed" || got.SelectedCommitSHA == nextRequest.HeadSHA {
		t.Fatal("newhead inherited old CI", got)
	}
	admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
	got = readSourceCI(t, f)
	if got.State != "held" || got.Reason != "source_configuration_changed" {
		t.Fatal(got)
	}
}
