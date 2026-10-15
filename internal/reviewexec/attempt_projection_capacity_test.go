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
	counts := []int{0, 100 - spare, 0}
	if total {
		counts = []int{50, 50 - spare, 100}
	}
	for n := range queue {
		for i := 0; i < counts[n]; i++ {
			queue[n].Attempts = append(queue[n].Attempts, reviews.Attempt{SourceAttemptID: fmt.Sprintf("imported-%d-%d", queue[n].Number, i), Channel: "cli", HeadSHA: queue[n].HeadSHA, State: "prepared"})
		}
	}
	request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "capacity-replacement", ObservedAt: stamp(), Queue: queue}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, fmt.Sprintf("replace-%d", revision), encoded(request))
	if w.Code != 201 {
		t.Fatal("canonical replacement", w.Code, w.Body.String())
	}
}

func TestAttemptProjectionReplacementCapacityRemainsLedgerOnly(t *testing.T) {
	for _, total := range []bool{false, true} {
		for _, boundary := range []string{"admit", "advance", "restart-preparing", "restart-active"} {
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
				// Explicit publication retains the normal bounded-history error and must
				// not leave the copied snapshot rows created before the capacity check.
				_, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil)
				attemptCode(t, err, "workspace_capacity")
				if projectionDigest(t, f, "") != before {
					t.Fatal("explicit failed publication left orphan projection rows")
				}
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
					t.Fatalf("journal transition at full projection: state=%s want=%s err=%v", a.State, want, err)
				}
				if projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != events {
					t.Fatal("ledger-only transition changed current or historical projection")
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

func TestAttemptProjectionExistingNativeUpdatesAtCapacity(t *testing.T) {
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
				t.Fatal("existing native row skipped at capacity")
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

func TestAttemptProjectionFullMissingRowStillRejectsCorruptJournal(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "corrupt-at-full", "cli", 2, 1))
	replaceAttemptCapacity(t, f, false, 0)
	admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at='invalid' WHERE attempt_id=? AND revision=1`, a.ID)
	before := projectionDigest(t, f, "")
	transitions := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`)
	_, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err == nil {
		t.Fatal("capacity swallowed corrupt journal timing")
	}
	if projectionDigest(t, f, "") != before || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions`) != transitions {
		t.Fatal("corrupt transition committed partial state")
	}
}
