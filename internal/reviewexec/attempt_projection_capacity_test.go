package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

// Use the public replacement contract: an administrator may legitimately omit
// a live native attempt while filling the current snapshot with imported history.
func replaceAttemptCapacity(t *testing.T, f *attemptFixture, total bool, spare int) {
	t.Helper()
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	queue := []reviews.PullInput{projectionPull(1), projectionPull(2), projectionPull(4)}
	queue[0].State = "merged"
	request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "replacement-before-transfer", ObservedAt: stamp(), Queue: queue}
	if w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, fmt.Sprintf("replace-%d", revision), encoded(request)); w.Code != 201 {
		t.Fatal("canonical replacement", w.Code, w.Body.String())
	}
	counts := []int{0, 101 - spare, 0}
	if total {
		counts = []int{51, 51 - spare, 101}
	}
	for batch := 0; ; batch++ {
		var selected []reviews.PullInput
		remaining := 200
		for n := range queue {
			if counts[n] <= 0 || remaining == 0 {
				continue
			}
			pull := queue[n]
			pull.Complete = false
			count := min(counts[n], 100, remaining)
			remaining -= count
			for i := 0; i < count; i++ {
				pull.Attempts = append(pull.Attempts, reviews.Attempt{SourceAttemptID: fmt.Sprintf("imported-%d-%d-%d", pull.Number, batch, i), Channel: "cli", HeadSHA: pull.HeadSHA, State: "prepared"})
			}
			counts[n] -= count
			selected = append(selected, pull)
		}
		if len(selected) == 0 {
			break
		}
		transferProjectionBatch(t, f, fmt.Sprintf("replacement-%d-%d", revision, batch), []reviews.TransferGroup{{Source: "capacity-replacement", Pulls: selected}})
	}
}

func TestAttemptProjectionReplacementGrowthProjectsJournalTransitions(t *testing.T) {
	for _, total := range []bool{false, true} {
		for _, boundary := range []string{"explicit", "admit", "advance", "restart-preparing", "restart-active"} {
			t.Run(fmt.Sprintf("total-%t/%s", total, boundary), func(t *testing.T) {
				f := attemptProjectionFixture(t, true)
				a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "capacity-live", "cli", 2, 1))
				if boundary == "advance" || boundary == "restart-active" {
					var err error
					a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
					if err != nil {
						t.Fatal(err)
					}
				}
				replaceAttemptCapacity(t, f, total, 0)
				before := projectionDigest(t, f, "")
				events := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`)
				transitions := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`)
				var prior reviews.PullResponse
				projectionRead(t, f, "/pulls/2", &prior)
				historical := projectionDigest(t, f, prior.SnapshotID)
				// Exercise explicit recovery independently. The automatic cases
				// below must restore an omitted attempt on their own transition.
				if boundary == "explicit" {
					receipt, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil)
					if err != nil || receipt.ID == "" || projectionDigest(t, f, "") == before || projectionDigest(t, f, prior.SnapshotID) != historical || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != events+1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != transitions {
						t.Fatal("explicit growth recovery changed journal or failed to project", receipt, err)
					}
					_, projected := projectedAttempt(t, f, a)
					if projected.State != a.State {
						t.Fatal("explicit growth state not retained")
					}
					after := projectionDigest(t, f, "")
					if replay, replayErr := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil); replayErr != nil || replay != receipt || projectionDigest(t, f, "") != after || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != events+1 {
						t.Fatal("explicit growth replay repeated effects", replayErr)
					}
					return
				}
				var err error

				want := "active"
				switch boundary {
				case "admit":
					a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
				case "advance":
					a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", "")
					want = "completed"
				default:
					// Start performs synchronous crash recovery before launching workers.
					restarted, newErr := New(f.store, attemptCapabilities, WithNativeProjection())
					if newErr != nil {
						t.Fatal(newErr)
					}
					ctx, cancel := context.WithCancel(t.Context())
					driver := &controlledDriver{}
					done, startErr := restarted.Start(ctx, map[string]Driver{"test-capability": driver, "other-account": driver})
					cancel()
					if startErr != nil {
						t.Fatal("restart recovery", startErr)
					}
					select {
					case stopErr := <-done:
						if stopErr != nil {
							t.Fatal(stopErr)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("supervisor shutdown timed out")
					}
					err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
						var readErr error
						a, readErr = readAttempt(ctx, tx, attemptProject, a.ID)
						return readErr
					})
					want = "uncertain"
					if boundary == "restart-preparing" {
						want = "cancelled"
					}
					if driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
						t.Fatal("recovery dispatched provider work")
					}
				}
				if err != nil || a.State != want {
					t.Fatalf("journal transition after retained growth: state=%s want=%s err=%v", a.State, want, err)
				}
				if projectionDigest(t, f, "") == before || projectionDigest(t, f, prior.SnapshotID) != historical || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != events+1 {
					t.Fatal("growth transition lost current update or rewrote pinned history")
				}
				_, projected := projectedAttempt(t, f, a)
				if projected.State != want {
					t.Fatal("growth transition not projected", projected)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != transitions+1 {
					t.Fatal("journal transition not retained exactly once")
				}
				reservations := 0
				if want == "active" || want == "uncertain" {
					reservations = 1
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != reservations {
					t.Fatal("journal reservation semantics changed")
				}
			})
		}
	}
}

func TestAttemptProjectionExistingNativeUpdatesAfterRetainedGrowth(t *testing.T) {
	for _, total := range []bool{false, true} {
		t.Run(fmt.Sprintf("total-%t", total), func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			replaceAttemptCapacity(t, f, total, 1)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "last-slot", "cli", 2, 1))
			var err error
			a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
			if err != nil {
				t.Fatal(err)
			}
			_, projected := projectedAttempt(t, f, a)
			if projected.State != "active" {
				t.Fatal("existing native row skipped after retained growth")
			}
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", "")
			if err != nil {
				t.Fatal(err)
			}
			_, projected = projectedAttempt(t, f, a)
			assertProjectedTimes(t, f, a, projected, true)
			before := projectionDigest(t, f, "")
			if _, err = f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil); err != nil || projectionDigest(t, f, "") != before {
				t.Fatal("explicit existing-row replay changed projection", err)
			}
		})
	}
}

func TestAttemptProjectionRetainedGrowthStillRejectsCorruptJournal(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "corrupt-at-full", "cli", 2, 1))
	replaceAttemptCapacity(t, f, false, 0)
	admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at='invalid' WHERE attempt_id=? AND revision=1`, a.ID)
	before := projectionDigest(t, f, "")
	transitions := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`)
	_, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err == nil {
		t.Fatal("retained growth swallowed corrupt journal timing")
	}
	if projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != transitions {
		t.Fatal("corrupt transition committed partial state")
	}
}

func missingTargetCapacityObservation(t *testing.T, f *attemptFixture, fullPulls, fullAttempts bool, existingTarget bool) {
	t.Helper()
	queue := []reviews.PullInput{projectionPull(4), projectionPull(6)}
	if fullPulls {
		queue = nil
		for i := int64(100); i < 200; i++ {
			queue = append(queue, projectionPull(i))
		}
		if existingTarget {
			queue[0] = projectionPull(2)
		}
	}
	if fullAttempts {
		for n := range 2 {
			for i := range 100 {
				queue[n].Attempts = append(queue[n].Attempts, reviews.Attempt{SourceAttemptID: fmt.Sprintf("retained-%d-%d", queue[n].Number, i), Channel: "cli", HeadSHA: queue[n].HeadSHA, State: "prepared"})
			}
		}
	}
	zero := int64(0)
	request := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 801, Source: "capacity-before-native-target", ObservedAt: stamp(), Queue: queue}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "initial-capacity", encoded(request))
	if w.Code != 201 {
		t.Fatal("canonical capacity observation", w.Code, w.Body.String())
	}
}

func TestAttemptProjectionMissingTargetGrowthAllowsPreparation(t *testing.T) {
	for _, kind := range []string{"global-attempts", "pull-rows"} {
		t.Run(kind, func(t *testing.T) {
			f := attemptProjectionFixture(t, false)
			missingTargetCapacityObservation(t, f, kind == "pull-rows", kind == "global-attempts", false)
			request := f.enqueue(t, attemptProject, "missing-target-at-capacity", "cli", 2, 1)
			before := projectionDigest(t, f, "")
			driver := &controlledDriver{prepare: func(ctx context.Context, _ Operation, _ Configuration, a Attempt) (Preparation, error) {
				metadata := projectionPull(2)
				if _, err := f.e.PublishAttempt(ctx, attemptProject, a.ID, &metadata); err != nil {
					return Preparation{}, err
				}
				return Preparation{Anchor: attemptAnchor(attemptProject, 2)}, nil
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), request.ID)
			if err != nil || operation.State != "completed" || operation.AttemptID == "" || driver.prepares.Load() != 1 || driver.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || projectionDigest(t, f, "") == before {
				t.Fatal("retained totals blocked preparation/new native PR", operation, err)
			}
			after := projectionDigest(t, f, "")
			events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			if err = f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil || projectionDigest(t, f, "") != after || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events || driver.prepares.Load() != 1 || driver.calls.Load() != 1 {
				t.Fatal("completed growth work repeated effects", err)
			}

		})
	}
}

func TestAttemptProjectionMissingTargetCapacityAllowsInitialMetadataWithinBounds(t *testing.T) {
	for _, kind := range []string{"unconfigured-workspace", "empty-workspace", "missing-spare", "existing-full-queue"} {
		t.Run(kind, func(t *testing.T) {
			f := attemptProjectionFixture(t, false)
			switch kind {
			case "empty-workspace":
				zero := int64(0)
				request := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 801, Source: "empty-history", ObservedAt: stamp(), Queue: []reviews.PullInput{}}
				if w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "empty", encoded(request)); w.Code != 201 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "missing-spare":
				missingTargetCapacityObservation(t, f, false, false, false)
			case "existing-full-queue":
				missingTargetCapacityObservation(t, f, true, false, true)
			}
			receipt := f.enqueue(t, attemptProject, "initial-metadata-with-capacity", "cli", 2, 1)
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, c Configuration, a Attempt) (Preparation, error) {
				metadata := projectionPull(2)
				if _, err := f.e.PublishAttempt(ctx, attemptProject, a.ID, &metadata); err != nil {
					return Preparation{}, err
				}
				return Preparation{Anchor: attemptAnchor(attemptProject, 2)}, nil
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), receipt.ID)
			if err != nil || operation.State != "completed" || driver.prepares.Load() != 1 || driver.calls.Load() != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("valid initial publication refused", operation, err, driver.prepares.Load(), driver.calls.Load())
			}
			_, projected := projectedAttempt(t, f, Attempt{ID: operation.AttemptID})
			if projected.State != "completed" {
				t.Fatal("initial metadata not published", projected)
			}
		})
	}
}

func TestAttemptProjectionMissingTargetReadFailuresRollbackBeforePreparation(t *testing.T) {
	for _, table := range []string{"review_pulls", "review_workspace"} {
		t.Run(table, func(t *testing.T) {
			f := attemptProjectionFixture(t, false)
			missingTargetCapacityObservation(t, f, false, false, false)
			receipt := f.enqueue(t, attemptProject, "storage-error-before-preparation", "cli", 2, 1)
			// Break only the owned fixture's read schema, preserving a genuine queued
			// operation so StartAttempt, rather than the pending query, reaches failure.
			kind := "VIEW"
			if table == "review_workspace" {
				kind = "TABLE"
			}
			admissionSQL(t, f, `DROP `+kind+` `+table)
			beforeEvents := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			a, err := f.e.StartAttempt(t.Context(), attemptProject, receipt.ID)
			if err == nil || a.ID != "" {
				t.Fatal("missing-target capacity swallowed storage failure", a, err)
			}
			operation, readErr := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), receipt.ID)
			if readErr != nil || operation.State != "queued" || operation.Revision != 1 || operation.AttemptID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != beforeEvents {
				t.Fatal("storage failure committed journal effects", operation, readErr)
			}
		})
	}
}
