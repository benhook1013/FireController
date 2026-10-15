package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func attemptProjectionFixture(t *testing.T, initial bool) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	for _, ensure := range []func(context.Context, *core.Store) error{reviews.EnsureSchema, EnsureEvidenceSchema, EnsureAdjudicationSchema} {
		if err := ensure(t.Context(), f.store); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	f.configure(t, attemptProject, "test-capability", 0, false)
	if initial {
		merged := projectionPull(1)
		merged.State = "merged"
		merged.Attempts = []reviews.Attempt{{SourceAttemptID: "retained-imported-attempt", Channel: "cli", HeadSHA: merged.HeadSHA, State: "prepared"}}
		request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "imported-history", ObservedAt: "2026-01-01T00:00:00Z", Queue: []reviews.PullInput{merged, projectionPull(2), projectionPull(4)}}
		w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "initial-workspace", encoded(request))
		if w.Code != 201 {
			t.Fatal("initial history", w.Code, w.Body.String())
		}
	}
	return f
}

func projectedAttempt(t *testing.T, f *attemptFixture, a Attempt) (reviews.PullResponse, reviews.Attempt) {
	t.Helper()
	var pull reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &pull)
	for _, value := range pull.Attempts {
		if value.SourceAttemptID == a.ID {
			return pull, reviews.Attempt(value)
		}
	}
	t.Fatal("journal attempt absent from browser", a.ID)
	return pull, reviews.Attempt{}
}

func assertProjectedTimes(t *testing.T, f *attemptFixture, a Attempt, value reviews.Attempt, terminal bool) {
	t.Helper()
	var started, finished string
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? ORDER BY revision LIMIT 1`, a.ID).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if value.StartedAt != started || value.State != a.State {
		t.Fatal("projected state/start did not retain journal", value)
	}
	if !terminal {
		if value.FinishedAt != "" || value.DurationSeconds != nil {
			t.Fatal("unproven finish fabricated", value)
		}
		return
	}
	first, _ := time.Parse(time.RFC3339Nano, started)
	last, _ := time.Parse(time.RFC3339Nano, finished)
	seconds := int64(last.Sub(first) / time.Second)
	if value.FinishedAt != finished || value.DurationSeconds == nil || *value.DurationSeconds != seconds {
		t.Fatal("terminal timing did not match journal", value)
	}
}

func TestAttemptProjectionTransitionsPreserveCurrentFactsAndHistory(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	var merged reviews.PullResponse
	projectionRead(t, f, "/pulls/1", &merged)
	originalDigest := projectionDigest(t, f, merged.SnapshotID)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "native-start", "cli", 2, 1))
	_, preparing := projectedAttempt(t, f, a)
	assertProjectedTimes(t, f, a, preparing, false)
	if preparing.SourceRunID != "" || preparing.ProviderReviewID != "" {
		t.Fatal("preparation fabricated capture attribution")
	}
	// A separate fresh metadata observation moves the current head and CI. The
	// next journal transition must retain it, while this attempt retains its head.
	moved := projectionPull(2)
	moved.HeadSHA = strings.Repeat("9", 40)
	moved.Title = "Current moved title"
	moved.CIState = "pending"
	moved.Mergeability = "conflicting"
	marker := reviews.Attempt{SourceAttemptID: "fresh-metadata-observation", Channel: "hosted", HeadSHA: moved.HeadSHA, State: "prepared"}
	projectionPublish(t, f, reviews.NativePublication{Pull: moved, Attempt: &marker})
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	pull, active := projectedAttempt(t, f, a)
	assertProjectedTimes(t, f, a, active, false)
	if pull.Pull.Title != moved.Title || pull.Pull.HeadSHA != moved.HeadSHA || pull.Pull.CIState != moved.CIState || pull.Pull.Mergeability != moved.Mergeability || active.HeadSHA != strings.Repeat("a", 40) || pull.ObservedAt != merged.ObservedAt {
		t.Fatal("attempt transition overwrote current PR facts or freshness", pull)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", "")
	if err != nil {
		t.Fatal(err)
	}
	_, completed := projectedAttempt(t, f, a)
	assertProjectedTimes(t, f, a, completed, true)
	var afterMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/1", &afterMerged)
	if !reflect.DeepEqual(merged.Pull, afterMerged.Pull) || !reflect.DeepEqual(merged.Attempts, afterMerged.Attempts) || originalDigest != projectionDigest(t, f, merged.SnapshotID) {
		t.Fatal("native journal projection changed imported/merged or historical evidence")
	}
	var actor string
	if err := f.store.DB().QueryRow(`SELECT actor_id FROM events WHERE event_type='review.observed' ORDER BY sequence DESC LIMIT 1`).Scan(&actor); err != nil || actor != "exec-writer-principal" {
		t.Fatal("native projection lost original actor", actor, err)
	}
}

func TestAttemptProjectionTerminalAndUncertainStates(t *testing.T) {
	for _, state := range []string{"held", "failed", "cancelled", "rate_limited", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "state-test", "cli", 2, 1))
			if state == "uncertain" {
				var err error
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
				if err != nil {
					t.Fatal(err)
				}
			}
			a, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, state, "local_state", "")
			if err != nil {
				t.Fatal(err)
			}
			_, value := projectedAttempt(t, f, a)
			assertProjectedTimes(t, f, a, value, state != "uncertain")
			if state == "held" {
				// The owning trusted publisher also refuses to reopen a held projection.
				value.State = "preparing"
				pull := projectionPull(2)
				err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					_, err := reviews.PublishNative(ctx, tx, core.Scope{ProjectID: attemptProject, PrincipalID: "test"}, reviews.NativePublication{Pull: pull, Attempt: &value})
					return err
				})
				attemptCode(t, err, "observation_conflict")
			}
		})
	}
}

func TestAttemptProjectionRecoveryAndHostedTrigger(t *testing.T) {
	for _, channel := range []string{"preparing", "cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			selected := "cli"
			if channel == "hosted" {
				selected = "hosted"
			}
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "recovery-test", selected, 2, 1))
			if channel != "preparing" {
				intent := ""
				if channel == "hosted" {
					intent = "explicit-intent"
				}
				var err error
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), intent)
				if err != nil {
					t.Fatal(err)
				}
			}
			if channel == "hosted" {
				_, posting := projectedAttempt(t, f, a)
				assertProjectedTimes(t, f, a, posting, false)
				var err error
				a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recorded", "comment-123")
				if err != nil {
					t.Fatal(err)
				}
				_, active := projectedAttempt(t, f, a)
				if active.TriggerID != "comment-123" || active.ProviderReviewID != "" {
					t.Fatal("hosted trigger conflated with provider review", active)
				}
			}
			if err := f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var err error
				a, err = readAttempt(ctx, tx, attemptProject, a.ID)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			_, value := projectedAttempt(t, f, a)
			assertProjectedTimes(t, f, a, value, channel == "preparing")
			want := "uncertain"
			if channel == "preparing" {
				want = "cancelled"
			}
			if value.State != want {
				t.Fatal("recovery projection state", value)
			}
			if channel == "hosted" && value.TriggerID != "comment-123" {
				t.Fatal("recovery lost known trigger")
			}
		})
	}
}

func TestAttemptProjectionMissingPRExplicitPublicationAndReplay(t *testing.T) {
	f := attemptProjectionFixture(t, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "ledger-only", "cli", 2, 1))
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 0 {
		t.Fatal("journal fabricated initial PR")
	}
	_, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil)
	attemptCode(t, err, "review_projection_missing")
	// Known retained times are provided by this owned fixture before the first
	// browser publication. Later transitions must never reset this start time.
	started := time.Now().Add(-3700 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=1`, started, a.ID)
	pull := projectionPull(2)
	first, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, &pull)
	if err != nil {
		t.Fatal(err)
	}
	before := projectionDigest(t, f, "")
	events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	// Fresh metadata is used for initial/changed publication, not to turn exact
	// attempt recovery into another observation or overwrite current facts.
	changed := pull
	changed.Title = "This replay metadata must not replace current title"
	for _, metadata := range []*reviews.PullInput{nil, &pull, &changed} {
		receipt, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, metadata)
		if err != nil || receipt != first || projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
			t.Fatal("attempt replay changed snapshot/event", receipt, err)
		}
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "done", "")
	if err != nil {
		t.Fatal(err)
	}
	_, value := projectedAttempt(t, f, a)
	assertProjectedTimes(t, f, a, value, true)
	if value.StartedAt != started || value.DurationSeconds == nil || *value.DurationSeconds < 3 {
		t.Fatal("original duration/start was replaced", value)
	}
}

func TestAttemptProjectionInitialFallbackPreservesInterveningCurrentObservation(t *testing.T) {
	f := attemptProjectionFixture(t, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "fallback-race", "hosted", 2, 1))
	_, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil)
	attemptCode(t, err, "review_projection_missing")
	// Another legitimate publisher establishes current facts after the caller's
	// missing-row read but before its initial-metadata fallback transaction.
	current, merged := projectionPull(2), projectionPull(1)
	current.Title, current.State, current.HeadSHA = "New merged observation", "merged", strings.Repeat("9", 40)
	current.BaseRef, current.BaseSHA, current.CIState = "current-base", strings.Repeat("8", 40), "failure"
	current.Mergeability = "unknown"
	current.Attempts = []reviews.Attempt{{SourceAttemptID: "intervening-imported-attempt", Channel: "cli", HeadSHA: current.HeadSHA, State: "prepared"}}
	merged.State = "merged"
	request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "intervening-import", ObservedAt: stamp(), Queue: []reviews.PullInput{merged, current}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "intervening-current-facts", encoded(request))
	if w.Code != 201 {
		t.Fatal("intervening import", w.Code, w.Body.String())
	}
	var initial, initialMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &initial)
	projectionRead(t, f, "/pulls/1", &initialMerged)
	history := projectionDigest(t, f, initial.SnapshotID)
	old := projectionPull(2)
	old.Title, old.CIState, old.Mergeability, old.Complete = "Older prepared plan", "unknown", "unknown", false
	receipt, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, &old)
	if err != nil {
		t.Fatal(err)
	}
	after, projected := projectedAttempt(t, f, a)
	if after.Pull.Title != current.Title || after.Pull.State != current.State || after.Pull.HeadSHA != current.HeadSHA || after.Pull.BaseRef != current.BaseRef || after.Pull.BaseSHA != current.BaseSHA || after.Pull.CIState != current.CIState || after.Pull.Mergeability != current.Mergeability || after.Pull.Complete != current.Complete || projected.HeadSHA != strings.Repeat("a", 40) || after.ObservedAt != initial.ObservedAt {
		t.Fatal("initial fallback overwrote intervening current PR facts", after.Pull)
	}
	if len(after.Attempts) != len(initial.Attempts)+1 || history != projectionDigest(t, f, initial.SnapshotID) {
		t.Fatal("fallback lost imported attempts or changed historical snapshot")
	}
	foundImported := false
	for _, value := range after.Attempts {
		if value.SourceAttemptID == "intervening-imported-attempt" {
			foundImported = reflect.DeepEqual(value, initial.Attempts[0])
		}
	}
	var afterMerged reviews.PullResponse
	projectionRead(t, f, "/pulls/1", &afterMerged)
	if !foundImported || !reflect.DeepEqual(initialMerged.Pull, afterMerged.Pull) {
		t.Fatal("fallback changed imported/merged history")
	}
	before, events := projectionDigest(t, f, ""), attemptCount(t, f.store, `SELECT count(*) FROM events`)
	if replay, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, &old); err != nil || replay != receipt || projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
		t.Fatal("fallback replay changed projection/events", replay, err)
	}
	for _, number := range []int64{0, 4} {
		invalid := old
		invalid.Number = number
		_, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, &invalid)
		attemptCode(t, err, "observation_conflict")
		if projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
			t.Fatal("invalid fallback selector had effects")
		}
	}
}

func TestAttemptProjectionCaptureLinkRequiresCurrentPublishedRun(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 0, "completed")
	pull := projectionPull(2)
	if _, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, &pull); err != nil {
		t.Fatal(err)
	}
	_, before := projectedAttempt(t, f, a)
	if before.SourceRunID != "" {
		t.Fatal("retained capture alone became published run link")
	}
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, nil); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil)
	if err != nil || receipt.ID == "" {
		t.Fatal(err)
	}
	_, after := projectedAttempt(t, f, a)
	if after.SourceRunID != evidence.ID || after.ProviderReviewID != "" {
		t.Fatal("actual published capture did not link", after)
	}
}

func TestAttemptProjectionFaultRollsBackJournalAndReservation(t *testing.T) {
	for _, boundary := range []string{"start", "admit", "advance", "recover"} {
		t.Run(boundary, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			r := f.enqueue(t, attemptProject, "fault-test", "cli", 2, 1)
			var a Attempt
			if boundary != "start" {
				a = f.start(t, attemptProject, r)
			}
			if boundary == "advance" {
				var err error
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
				if err != nil {
					t.Fatal(err)
				}
			}
			before := projectionDigest(t, f, "")
			events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			transitions := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`)
			attempts := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`)
			reservations := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`)
			operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `CREATE TRIGGER reject_attempt_projection BEFORE INSERT ON review_snapshots WHEN NEW.source='native-executor' BEGIN SELECT RAISE(ABORT,'synthetic projection failure'); END`)
			switch boundary {
			case "start":
				_, err = f.e.StartAttempt(t.Context(), attemptProject, r.ID)
			case "admit":
				_, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
			case "advance":
				_, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "done", "")
			case "recover":
				err = f.e.RecoverInterrupted(t.Context())
			}
			if err == nil || strings.Contains(err.Error(), "synthetic") {
				t.Fatal("projection fault not safely reported", err)
			}
			after, readErr := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), r.ID)
			if readErr != nil || !reflect.DeepEqual(operation, after) || before != projectionDigest(t, f, "") || events != attemptCount(t, f.store, `SELECT count(*) FROM events`) || transitions != attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) || attempts != attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) || reservations != attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) {
				t.Fatal("projection fault committed partial journal/projection", readErr)
			}
		})
	}
}

func TestAttemptProjectionCapacityHoldsOnceBeforeDriver(t *testing.T) {
	for _, total := range []bool{false, true} {
		t.Run(fmt.Sprint("total-", total), func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			// Populate only compact rows in the owned current snapshot, not hundreds of
			// whole snapshots. The existing imported row already contributes one item.
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var snapshot string
				if err := tx.QueryRowContext(ctx, `SELECT snapshot_id FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&snapshot); err != nil {
					return err
				}
				for number, count := range map[int]int{2: 100, 4: 0} {
					if total {
						if number == 2 {
							count = 99
						} else {
							count = 100
						}
					}
					for i := 0; i < count; i++ {
						if _, err := tx.ExecContext(ctx, `INSERT INTO review_attempts(project_id,snapshot_id,pull_number,position,source,source_attempt_id,channel,head_sha,state,started_at,finished_at,duration_seconds,source_run_id,trigger_id,provider_review_id) VALUES(?,?,?,?,'synthetic-history',?,'cli',?,'prepared','','',NULL,'','','')`, attemptProject, snapshot, number, i, fmt.Sprintf("retained-%d-%d", number, i), strings.Repeat("a", 40)); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			r := f.enqueue(t, attemptProject, "capacity-request", "cli", 2, 1)
			driver := &controlledDriver{}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), r.ID)
			if err != nil || operation.State != "held" || operation.Reason != "projection_capacity" || operation.AttemptID != "" {
				t.Fatal("full attempt history not held", operation, err)
			}
			before := projectionDigest(t, f, "")
			events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			again, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), r.ID)
			if err != nil || !reflect.DeepEqual(operation, again) || projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events || driver.prepares.Load() != 0 || driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("capacity hold retried writes or reached provider", err)
			}
		})
	}
}
