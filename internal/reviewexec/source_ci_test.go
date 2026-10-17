package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	admissionSQL(t, f, `DROP INDEX review_source_refresh_ci_target`)
	admissionSQL(t, f, `DROP INDEX review_source_refresh_coalesce`)
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
		"started_before_source": func(r *sourceCITestReader) {
			r.ci = func(ctx context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				o := sourceCITestObservation(target, "satisfied")
				o.StartedAt = o.StartedAt.Add(-time.Hour)
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
		"zero_started": func(s string) string {
			var o reviewprovider.CIObservation
			if err := json.Unmarshal([]byte(s), &o); err != nil {
				t.Fatal(err)
			}
			o.StartedAt = time.Time{}
			return encoded(o)
		},
		"zero_observed": func(s string) string {
			var o reviewprovider.CIObservation
			if err := json.Unmarshal([]byte(s), &o); err != nil {
				t.Fatal(err)
			}
			o.ObservedAt = time.Time{}
			return encoded(o)
		},
		"null": func(s string) string { return "null" },
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

func TestSourceCIRetainedFutureAndReversedObservationClocksRemainReadable(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &sourceCITestReader{}
	receipt := refreshEnqueue(t, f, "retained-ci-clock", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	operation := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	var raw string
	if err := f.store.DB().QueryRow(`SELECT ci_observation_json FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ci reviewprovider.CIObservation
	if err := json.Unmarshal([]byte(raw), &ci); err != nil {
		t.Fatal(err)
	}
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	source := *operation.Observation
	source.ObservedAt = future.Format(time.RFC3339Nano)
	ci.StartedAt = future.Add(time.Second)
	ci.ObservedAt = future.Add(2 * time.Second)
	for _, reversed := range []bool{false, true} {
		if reversed {
			ci.StartedAt = future.Add(-time.Second)
			ci.ObservedAt = future.Add(-2 * time.Second)
		}
		admissionSQL(t, f, `UPDATE review_source_refreshes SET observation_json=?,ci_observation_json=? WHERE id=?`, encoded(source), encoded(ci), receipt.ID)
		before := sourceCITestState(t, f)
		read, err := f.e.ReadSourceRefresh(t.Context(), sourceCIScope(t, f), receipt.ID)
		if err != nil || read.CI == nil || read.CI.State != "satisfied" || read.Observation.ObservedAt != source.ObservedAt {
			t.Fatal("retained CI rejected after wall-clock rollback", read, err)
		}
		assessment, err := f.e.ReadSourceCIAssessment(t.Context(), sourceCIScope(t, f), 2)
		if err != nil || assessment.State != "satisfied" || assessment.ObservedAt != ci.ObservedAt.Format(time.RFC3339Nano) || assessment.SourceObservedAt != source.ObservedAt {
			t.Fatal("CI assessment changed retained applicability", assessment, err)
		}
		policy, err := f.e.ReadSourcePolicyAssessment(t.Context(), sourceCIScope(t, f), 2)
		if err != nil || policy.SourceObservedAt != source.ObservedAt {
			t.Fatal("CI decoding blocked local policy", policy, err)
		}
		if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
			t.Fatal("retained reads wrote or fetched")
		}
	}
}

func TestSourceCIFreshFutureObservationRemainsUnproven(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		o := sourceCITestObservation(target, "satisfied")
		o.StartedAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		o.ObservedAt = o.StartedAt
		return o, nil
	}}
	receipt := refreshEnqueue(t, f, "fresh-future", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	operation := refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	if operation.CI == nil || operation.CI.State != "unproven" || operation.CI.Reason != "ci_observation_unproven" || operation.Observation == nil {
		t.Fatal("future ingestion gained credit", operation)
	}
}

func TestSourceCIRequiredContextCapacityRetainsExplicitReason(t *testing.T) {
	for _, name := range []string{"capacity129", "supported128", "foreign_target", "foreign_authority", "wrong_reason", "malformed_context", "malformed_app", "unexpected_results", "future_clock", "byte_capacity_first"} {
		t.Run(name, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				n := 129
				if name == "supported128" {
					n = 128
				}
				now := time.Now().UTC()
				checks := make([]reviewprovider.RequiredCheck, n)
				for i := range checks {
					checks[i] = reviewprovider.RequiredCheck{Context: fmt.Sprintf("context-%03d", i), AppID: 0}
				}
				o := reviewprovider.CIObservation{Target: target, State: "unproven", Reason: "ci_required_context_capacity", StartedAt: now, ObservedAt: now, Authority: reviewprovider.RequiredCheckAuthority{State: "complete", Repository: target.Repository, Branch: target.BaseRef, ObservedAt: now, Checks: checks}}
				switch name {
				case "supported128":
					o = sourceCITestObservation(target, "satisfied")
					o.Authority.Checks = checks
					o.Results.Statuses = nil
					for i, check := range checks {
						o.Results.Statuses = append(o.Results.Statuses, reviewprovider.CICommitStatus{ID: int64(i + 1), Context: check.Context, State: "success", CreatedAt: now, UpdatedAt: now})
					}
					var err error
					o.Assessment, err = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
					if err != nil {
						t.Fatal(err)
					}
					o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
				case "foreign_target":
					o.Target.RepositoryID++
				case "foreign_authority":
					o.Authority.Repository = "other/repo"
				case "wrong_reason":
					o.Reason = "ci_authority_unproven"
				case "malformed_context":
					o.Authority.Checks[0].Context = "\x00invalid"
				case "malformed_app":
					o.Authority.Checks[0].AppID = -1
				case "unexpected_results":
					o.Results = &reviewprovider.CIResultInventory{Repository: "other/repo"}
				case "future_clock":
					o.ObservedAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
				case "byte_capacity_first":
					o.Authority.Reason = strings.Repeat("x", sourceCIMaxBytes)
				}
				return o, nil
			}}
			receipt := refreshEnqueue(t, f, "contexts", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			got := refreshWait(t, f, receipt.ID, "completed")
			refreshStop(t, cancel, done)
			wantState, wantReason, wantRequired := "unproven", "ci_observation_unproven", int64(0)
			switch name {
			case "capacity129", "malformed_context", "malformed_app", "unexpected_results":
				// Discarded inventory does not need certification to report
				// capacity; no context, result or passing count is retained.
				wantReason = "ci_required_context_capacity"
			case "supported128":
				wantState, wantReason, wantRequired = "satisfied", "", 128
			case "byte_capacity_first":
				wantReason = "ci_observation_capacity"
			}
			if got.Observation == nil || got.CI == nil || got.CI.State != wantState || got.CI.Reason != wantReason || got.CI.Required != wantRequired {
				t.Fatal("CI capacity lost or gained credit", got)
			}
			var raw string
			if err := f.store.DB().QueryRow(`SELECT ci_observation_json FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if name != "supported128" && (len(raw) > 4096 || strings.Contains(raw, "context-")) {
				t.Fatal("oversized or malformed inventory retained", len(raw))
			}
			before, calls, ciCalls := sourceCITestState(t, f), reader.calls.Load(), reader.ciCalls.Load()
			assessment := readSourceCI(t, f)
			if assessment.State != wantState || assessment.Reason != wantReason || assessment.Required != wantRequired {
				t.Fatal("local read changed capacity reason", assessment)
			}
			path := "/api/v1/projects/" + attemptProject + "/reviews/source/2/ci"
			w := f.request(http.MethodGet, path, f.reader, "", "")
			var public SourceCIAssessment
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &public) != nil || public.State != wantState || public.Reason != wantReason || public.Required != wantRequired {
				t.Fatal("HTTP lost capacity reason", w.Code, w.Body.String())
			}
			if sourceCITestState(t, f) != before || reader.calls.Load() != calls || reader.ciCalls.Load() != ciCalls || ciCalls != 1 {
				t.Fatal("local read wrote or fetched")
			}
		})
	}
}

func TestSourceCIPolicyReadsMetadataWithoutUnrelatedCIInventory(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &sourceCITestReader{}
	receipt := refreshEnqueue(t, f, "policy-ci-isolation", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	scope := sourceCIScope(t, f)
	expected, err := f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil {
		t.Fatal(err)
	}
	if expected.State != "held" || len(expected.Channels) != 2 {
		t.Fatal("fixture lacks policy result", expected)
	}
	calls, ciCalls := reader.calls.Load(), reader.ciCalls.Load()
	var ciBody string
	if err = f.store.DB().QueryRow(`SELECT ci_observation_json FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&ciBody); err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []string{"{", strings.Repeat("x", sourceCIMaxBytes+1)} {
		admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=? WHERE id=?`, corrupt, receipt.ID)
		before := sourceCITestState(t, f)
		got, err := f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
		if err != nil {
			t.Fatal("policy read decoded unrelated CI", err)
		}
		got.EvaluatedAt = expected.EvaluatedAt
		if !reflect.DeepEqual(got, expected) {
			t.Fatal("CI corruption changed policy", got, expected)
		}
		w := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/source/2/policy", f.reader, "", "")
		var public SourcePolicyAssessment
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &public) != nil {
			t.Fatal("policy HTTP decoded unrelated CI", w.Code, w.Body.String())
		}
		public.EvaluatedAt = expected.EvaluatedAt
		if !reflect.DeepEqual(public, expected) {
			t.Fatal("policy HTTP changed result", public, expected)
		}
		_, err = f.e.ReadSourceCIAssessment(t.Context(), scope, 2)
		attemptCode(t, err, "storage_unavailable")
		_, err = f.e.ReadSourceRefresh(t.Context(), scope, receipt.ID)
		attemptCode(t, err, "storage_unavailable")
		for _, path := range []string{"/reviews/source/2/ci", "/reviews/source-refreshes/" + receipt.ID} {
			if w = f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+path, f.reader, "", ""); w.Code != http.StatusInternalServerError {
				t.Fatal("explicit CI read bypassed corruption", w.Code, w.Body.String())
			}
		}
		if sourceCITestState(t, f) != before || reader.calls.Load() != calls || reader.ciCalls.Load() != ciCalls {
			t.Fatal("local read wrote or fetched")
		}
	}
	admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json=?,capability_fingerprint='malformed' WHERE id=?`, ciBody, receipt.ID)
	before := sourceCITestState(t, f)
	_, err = f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	attemptCode(t, err, "storage_unavailable")
	if w := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/source/2/policy", f.reader, "", ""); w.Code != http.StatusInternalServerError {
		t.Fatal("metadata corruption escaped policy validation", w.Code, w.Body.String())
	}
	if sourceCITestState(t, f) != before || reader.calls.Load() != calls || reader.ciCalls.Load() != ciCalls {
		t.Fatal("corrupt metadata read wrote or fetched")
	}
}

func TestSourceCIComparisonReadsOnlyLatestSourceMetadata(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_%t", changed), func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			reader := &sourceCITestReader{}
			selected := refreshEnqueue(t, f, "selected-ci", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			old := refreshWait(t, f, selected.ID, "completed")
			refreshStop(t, cancel, done)
			latest := refreshEnqueue(t, f, "latest-source", refreshRequest())
			if changed {
				reader.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					return reviewprovider.Pull{RepositoryID: 801, Number: n, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("d", 40), BaseRef: "main", ChangedFiles: 1}, nil
				}
			}
			cancel, done = refreshStart(t, f, reader)
			refreshWait(t, f, latest.ID, "completed")
			refreshStop(t, cancel, done)
			// Pin which collected row is latest without relying on host wall-clock order.
			at, err := time.Parse(time.RFC3339Nano, old.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE review_source_refreshes SET created_at=?,ci_observation_json='malformed-unrelated-inventory' WHERE id=?`, at.Add(time.Second).UTC().Format(time.RFC3339Nano), latest.ID)
			before, calls, ciCalls := sourceCITestState(t, f), reader.calls.Load(), reader.ciCalls.Load()
			got, err := f.e.ReadSourceCIAssessment(t.Context(), sourceCIScope(t, f), 2)
			wantState, wantReason := "satisfied", ""
			if changed {
				wantState, wantReason = "held", "source_comparison_changed"
			}
			if err != nil || got.RefreshID != selected.ID || got.State != wantState || got.Reason != wantReason || got.Satisfied != 1 || got.Target != *old.Observation {
				t.Fatal("unrelated latest CI affected selected CI", got, err)
			}
			w := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/source/2/ci", f.reader, "", "")
			var public SourceCIAssessment
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &public) != nil || public.RefreshID != selected.ID || public.State != wantState || public.Reason != wantReason || public.Satisfied != 1 {
				t.Fatal("CI HTTP read decoded latest unrelated inventory", w.Code, w.Body.String())
			}
			if before != sourceCITestState(t, f) || calls != reader.calls.Load() || ciCalls != reader.ciCalls.Load() || ciCalls != 1 {
				t.Fatal("CI assessment wrote or fetched")
			}
			// Metadata-only comparison does not make the selected CI optional.
			admissionSQL(t, f, `UPDATE review_source_refreshes SET ci_observation_json='malformed-selected-inventory' WHERE id=?`, selected.ID)
			_, err = f.e.ReadSourceCIAssessment(t.Context(), sourceCIScope(t, f), 2)
			attemptCode(t, err, "storage_unavailable")
		})
	}
}
