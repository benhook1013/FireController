package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func sourceRetryFixture(t *testing.T) (*attemptFixture, Run) {
	t.Helper()
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}})
	r := schedulingLatest(t, f)
	if r.State != "held" || r.Revision != 3 || r.OperationID != "" {
		t.Fatal(r)
	}
	return f, r
}
func ageSourceRetry(t *testing.T, f *attemptFixture, id string) {
	t.Helper()
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := f.store.DB().Exec(`UPDATE review_selections SET created_at=?,updated_at=? WHERE id=?`, at, at, id); err != nil {
		t.Fatal(err)
	}
}
func retrySourceDirect(t *testing.T, f *attemptFixture, r Run, now time.Time) bool {
	t.Helper()
	var worked bool
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var basis string
		if err := tx.QueryRowContext(ctx, `SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?`, r.ID).Scan(&basis); err != nil {
			return err
		}
		s, err := readScheduling(ctx, tx, r.ProjectID)
		if err != nil {
			return err
		}
		worked, err = f.e.retryAutomaticSourceTx(ctx, tx, r.ProjectID, basis, s.Revision, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return worked
}
func TestAutomaticSourceRetryDelayedSameIntentSingleChild(t *testing.T) {
	f, original := sourceRetryFixture(t)
	schedulingGenerate(t, f, false)
	if got := schedulingLatest(t, f); got.Revision != original.Revision {
		t.Fatal(got)
	}
	ageSourceRetry(t, f, original.ID)
	schedulingGenerate(t, f, true)
	got := schedulingLatest(t, f)
	if got.ID != original.ID || got.State != "queued" || got.Revision != 4 || got.ActorID != original.ActorID || got.WorkerID != original.WorkerID || got.ConfigurationRevision != original.ConfigurationRevision || got.PolicyRevision != original.PolicyRevision {
		t.Fatal(got)
	}
	schedulingGenerate(t, f, false)
	selectionNext(t, f, &refreshTestReader{})
	got = schedulingLatest(t, f)
	if got.State != "selected" || got.OperationID == "" || got.ID != original.ID {
		t.Fatal(got)
	}
	schedulingGenerate(t, f, false)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("retry duplicated intent or started provider work")
	}
}
func TestAutomaticSourceRetryBudgetSurvivesEngineRestart(t *testing.T) {
	f, original := sourceRetryFixture(t)
	reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}}
	for _, expected := range []int64{6, 9} {
		ageSourceRetry(t, f, original.ID)
		restarted, err := New(f.store, attemptCapabilities)
		if err != nil {
			t.Fatal(err)
		}
		f.e = restarted
		if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
			t.Fatal(err)
		}
		schedulingGenerate(t, f, true)
		selectionNext(t, f, reader)
		if got := schedulingLatest(t, f); got.ID != original.ID || got.Revision != expected || got.State != "held" {
			t.Fatal(got)
		}
	}
	ageSourceRetry(t, f, original.ID)
	for range 3 {
		schedulingGenerate(t, f, false)
		if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if reader.calls.Load() != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("retry budget reset")
	}
}
func TestAutomaticSourceRetryReasonAllowlist(t *testing.T) {
	for _, reason := range []string{"source_budget_exhausted", "source_fetch_timeout", "source_provider_unavailable", "source_rate_limited", "source_unauthorized", "source_not_found", "source_response_invalid", "source_unavailable", "restore_abandoned", "selection_changed", "provider_unavailable", "cancelled", "source_reader_unavailable"} {
		t.Run(reason, func(t *testing.T) {
			f, r := sourceRetryFixture(t)
			if _, err := f.store.DB().Exec(`UPDATE review_selections SET reason=? WHERE id=?`, reason, r.ID); err != nil {
				t.Fatal(err)
			}
			want := reason == "source_budget_exhausted" || reason == "source_fetch_timeout" || reason == "source_provider_unavailable"
			if got := retrySourceDirect(t, f, r, time.Now().Add(time.Minute)); got != want {
				t.Fatal(reason, got)
			}
		})
	}
}
func TestAutomaticSourceRetryFences(t *testing.T) {
	for _, mode := range []string{"configuration", "policy", "authorization", "worker", "scheduling", "newer-intent", "explicit", "budget", "capability"} {
		t.Run(mode, func(t *testing.T) {
			f, r := sourceRetryFixture(t)
			var basis string
			var schedulingRevision int64
			switch mode {
			case "configuration":
				f.configure(t, attemptProject, "test-capability", 1, false)
			case "policy":
				assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
			case "authorization":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "worker":
				admissionSQL(t, f, `DELETE FROM workers WHERE project_id='`+attemptProject+`' AND id='selection-worker'`)
			case "scheduling":
				schedulingEnable(t, f, 1, true)
			case "newer-intent":
				selectionEnqueue(t, f, "later-intent", "hosted", nil)
			case "explicit":
				if err := f.store.DB().QueryRow(`SELECT settled_basis_sha256,scheduling_revision FROM review_automatic_selections WHERE selection_id=?`, r.ID).Scan(&basis, &schedulingRevision); err != nil {
					t.Fatal(err)
				}
				admissionSQL(t, f, `DELETE FROM review_automatic_selections WHERE selection_id='`+r.ID+`'`)
			case "budget":
				admissionSQL(t, f, `UPDATE review_selections SET revision=9 WHERE id='`+r.ID+`'`)
			case "capability":
				admissionSQL(t, f, `UPDATE review_selections SET capability_fingerprint='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id='`+r.ID+`'`)
			}
			if mode == "explicit" {
				ageSourceRetry(t, f, r.ID)
				// Direct helper cannot find a retained automatic intent for this selection.
				err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					worked, err := f.e.retryAutomaticSourceTx(ctx, tx, attemptProject, basis, schedulingRevision, time.Now())
					if worked {
						t.Error("explicit intent retried")
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			} else if retrySourceDirect(t, f, r, time.Now().Add(time.Minute)) {
				t.Fatal("fence ignored", mode)
			}
		})
	}
}
func TestAutomaticSourceRetryConcurrentTicks(t *testing.T) {
	f, r := sourceRetryFixture(t)
	ageSourceRetry(t, f, r.ID)
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			worked, err := f.e.coordinateAutomaticHosted(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
			results <- worked
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	count := 0
	for worked := range results {
		if worked {
			count++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 || schedulingLatest(t, f).Revision != 4 {
		t.Fatal("concurrent ticks duplicated recovery", count)
	}
}

func TestAutomaticSourceRetryInterruptedClaimConsumesBudget(t *testing.T) {
	f, r := sourceRetryFixture(t)
	ageSourceRetry(t, f, r.ID)
	schedulingGenerate(t, f, true)
	// Crash after claiming this recovery but before completing its source read.
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		current, err := readRun(ctx, tx, r.ProjectID, r.ID)
		if err != nil {
			return err
		}
		return updateRun(ctx, tx, current, "selecting", "", "")
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	f.e = restarted
	if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := schedulingLatest(t, f); got.Revision != 6 || got.State != "queued" {
		t.Fatal(got)
	}
	selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}})
	if got := schedulingLatest(t, f); got.Revision != 8 || got.State != "held" {
		t.Fatal(got)
	}
	ageSourceRetry(t, f, r.ID)
	schedulingGenerate(t, f, false)
}

func TestAutomaticSourceRetryFencedByLaterOperation(t *testing.T) {
	for _, state := range []string{"queued", "uncertain", "cancelled", "held", "completed"} {
		t.Run(state, func(t *testing.T) {
			f, r := sourceRetryFixture(t)
			ageSourceRetry(t, f, r.ID)
			raw, err := f.e.Enqueue(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "new-direct-operation", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), Channel: "hosted"})
			if err != nil {
				t.Fatal(err)
			}
			var receipt Receipt
			if err = json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			if state == "cancelled" {
				_, err = f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-later-intent", receipt.ID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
				if err != nil {
					t.Fatal(err)
				}
			} else if state != "queued" {
				admissionSQL(t, f, `UPDATE review_operations SET state=? WHERE id=?`, state, receipt.ID)
			}
			if retrySourceDirect(t, f, r, time.Now()) {
				t.Fatal("later operation ignored", state)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
				t.Fatal("operation duplicated")
			}
		})
	}
}

func TestAutomaticSourceRetryTransactionRollbackDoesNotConsumeBudget(t *testing.T) {
	f, r := sourceRetryFixture(t)
	ageSourceRetry(t, f, r.ID)
	admissionSQL(t, f, `CREATE TRIGGER retry_failure BEFORE INSERT ON events WHEN NEW.event_type='review.selection_updated' AND json_extract(NEW.payload_json,'$.state')='queued' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
	worked, err := f.e.generateAutomaticHosted(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
	if err == nil || worked {
		t.Fatal("failed transaction appeared successful", worked, err)
	}
	if got := schedulingLatest(t, f); got.State != "held" || got.Revision != 3 {
		t.Fatal(got)
	}
	admissionSQL(t, f, `DROP TRIGGER retry_failure`)
	schedulingGenerate(t, f, true)
	if got := schedulingLatest(t, f); got.ID != r.ID || got.Revision != 4 {
		t.Fatal(got)
	}
}

func TestAutomaticSourceRetryCoordinatorSafetyHolds(t *testing.T) {
	for _, mode := range []string{"disabled", "paused", "restore", "inactive"} {
		t.Run(mode, func(t *testing.T) {
			f, r := sourceRetryFixture(t)
			ageSourceRetry(t, f, r.ID)
			switch mode {
			case "disabled":
				schedulingEnable(t, f, 1, false)
			case "paused":
				schedulingPause(t, f, 2, 0, true)
			case "restore":
				admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('restore:provider_state','held')`)
			case "inactive":
				admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('restore:state','inactive')`)
			}
			schedulingGenerate(t, f, false)
			if got := schedulingLatest(t, f); got.ID != r.ID || got.State != "held" || got.Revision != r.Revision {
				t.Fatal(got)
			}
		})
	}
}

func TestAutomaticSourceRetryTimestampPrecisionFence(t *testing.T) {
	f, r := sourceRetryFixture(t)
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	admissionSQL(t, f, `UPDATE review_selections SET created_at=?,updated_at=? WHERE id=?`, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), r.ID)
	later, _ := selectionEnqueue(t, f, "later-fractional-time", "hosted", nil)
	// Exercise the time fallback independently of event acceptance order.
	admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.selection_requested' AND resource_id=?`, later.ID)
	admissionSQL(t, f, `UPDATE review_selections SET created_at=?,updated_at=? WHERE id=?`, at.Add(time.Millisecond).Format(time.RFC3339Nano), at.Add(time.Millisecond).Format(time.RFC3339Nano), later.ID)
	if retrySourceDirect(t, f, r, time.Now()) {
		t.Fatal("variable timestamp precision lost newer-intent fence")
	}
}

func TestAutomaticSourceRetryRechecksFencesAfterSourceIO(t *testing.T) {
	for _, mode := range []string{"cancelled-operation", "scheduling", "configuration", "policy"} {
		t.Run(mode, func(t *testing.T) {
			f, r := sourceRetryFixture(t)
			ageSourceRetry(t, f, r.ID)
			schedulingGenerate(t, f, true)
			var once sync.Once
			reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
				once.Do(func() {
					switch mode {
					case "cancelled-operation":
						raw, err := f.e.Enqueue(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "during-source", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), Channel: "hosted"})
						if err != nil {
							t.Fatal(err)
						}
						var receipt Receipt
						if err = json.Unmarshal(raw, &receipt); err != nil {
							t.Fatal(err)
						}
						_, err = f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-during-source", receipt.ID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
						if err != nil {
							t.Fatal(err)
						}
					case "scheduling":
						schedulingEnable(t, f, 1, true)
					case "configuration":
						f.configure(t, attemptProject, "test-capability", 1, false)
					case "policy":
						assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
					}
				})
				return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, r.ID)
			if got.State != "held" || got.OperationID != "" {
				t.Fatal("changed authority or cancelled successor ignored", got)
			}
			expected := 0
			if mode == "cancelled-operation" {
				expected = 1
				if got.Reason != "selection_superseded" {
					t.Fatal(got)
				}
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != expected {
				t.Fatal("retry created a child after its fence changed")
			}
		})
	}
}

func TestAutomaticSourceRetryAcceptanceOrderSurvivesClockRollback(t *testing.T) {
	f, r := sourceRetryFixture(t)
	ageSourceRetry(t, f, r.ID)
	later, _ := selectionEnqueue(t, f, "later-with-clock-rollback", "hosted", nil)
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	admissionSQL(t, f, `UPDATE review_selections SET created_at=?,updated_at=? WHERE id=?`, at, at, later.ID)
	if retrySourceDirect(t, f, r, time.Now()) {
		t.Fatal("wall-clock rollback erased a later accepted intent")
	}
}
