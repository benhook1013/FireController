package reviewexec

import (
	"math"
	"testing"
)

// The choose/finish refusal paths pass the incremented in-memory run rather
// than the normalized SQL claim. Exercise that exact MaxInt64 boundary.
func TestRecovery0030QuarantinesInMemoryMaximumRevision(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "automatic"}[automatic], func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			var r Run
			if automatic {
				schedulingEnable(t, f, 0, true)
				schedulingGenerate(t, f, true)
				r = schedulingLatest(t, f)
			} else {
				receipt, _ := selectionEnqueue(t, f, "max-claim", "hosted", nil)
				r = selectionRead(t, f, receipt.ID)
			}
			admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=? WHERE id=?`, int64(math.MaxInt64), r.ID)
			r.State, r.Revision = "selecting", math.MaxInt64
			history := snapshotTable(t, f, "events")
			attribution := snapshotTable(t, f, "review_automatic_selections")
			waiter := &operationWaiter{changed: make(chan struct{}, 1)}
			f.e.operationMu.Lock()
			if f.e.operationWaiters == nil {
				f.e.operationWaiters = map[string]map[*operationWaiter]struct{}{}
			}
			f.e.operationWaiters[r.ID] = map[*operationWaiter]struct{}{waiter: {}}
			f.e.operationMu.Unlock()
			worked, err := f.e.holdRunLocalRefusal(t.Context(), r)
			if err != nil || !worked {
				t.Fatal("in-memory max revision was not quarantined", worked, err)
			}
			got := selectionRead(t, f, r.ID)
			if got.State != "held" || got.Reason != "local_read_unavailable" || got.Revision != math.MaxInt64 || got.OperationID != "" {
				t.Fatal("quarantine rewrote retained evidence", got)
			}
			select {
			case <-waiter.changed:
			default:
				t.Fatal("waiter not notified")
			}
			if worked, err = f.e.holdRunLocalRefusal(t.Context(), r); err != nil || worked {
				t.Fatal("stale claim mutated quarantine", worked, err)
			}
			if history != snapshotTable(t, f, "events") || attribution != snapshotTable(t, f, "review_automatic_selections") {
				t.Fatal("quarantine fabricated history or replaced authority")
			}
		})
	}
}

func TestRecovery0030MaximumRevisionQuarantinePreservesFailuresAndCAS(t *testing.T) {
	for _, fault := range []string{"stale-revision", "write-failure"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			receipt, _ := selectionEnqueue(t, f, "max-claim-cas", "hosted", nil)
			r := selectionRead(t, f, receipt.ID)
			admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=? WHERE id=?`, int64(math.MaxInt64), r.ID)
			r.State, r.Revision = "selecting", math.MaxInt64
			if fault == "stale-revision" {
				r.Revision--
			} else {
				admissionSQL(t, f, `CREATE TRIGGER reject_quarantine0030 BEFORE UPDATE ON review_selections WHEN NEW.state='held' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
			}
			before := snapshotTable(t, f, "review_selections")
			worked, err := f.e.holdRunLocalRefusal(t.Context(), r)
			if worked || (err != nil) != (fault == "write-failure") {
				t.Fatal("lost failure or compare-and-swap", worked, err)
			}
			if before != snapshotTable(t, f, "review_selections") {
				t.Fatal("failed quarantine changed row")
			}
		})
	}
}

func TestRecovery0030DamagedPendingClaimIsIsolated(t *testing.T) {
	for _, state := range []string{"queued", "selecting"} {
		for _, fault := range []string{"revision-type", "maximum-revision", "fingerprint-blob"} {
			t.Run(state+"/"+fault, func(t *testing.T) {
				f := schedulingFixtureWithQueue(t, []int64{2})
				receipt, _ := selectionEnqueue(t, f, "damaged-claim", "hosted", nil)
				admissionSQL(t, f, `UPDATE review_selections SET state=? WHERE id=?`, state, receipt.ID)
				switch fault {
				case "revision-type":
					admissionSQL(t, f, `UPDATE review_selections SET revision='broken' WHERE id=?`, receipt.ID)
				case "maximum-revision":
					admissionSQL(t, f, `UPDATE review_selections SET revision=? WHERE id=?`, int64(math.MaxInt64), receipt.ID)
				case "fingerprint-blob":
					admissionSQL(t, f, `UPDATE review_selections SET capability_fingerprint=CAST(capability_fingerprint AS BLOB) WHERE id=?`, receipt.ID)
				}
				reader := &refreshTestReader{}
				var err error
				if state == "selecting" {
					err = f.e.RecoverReviewRuns(t.Context())
				} else {
					_, err = f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": reader})
				}
				if err != nil {
					t.Fatal("damaged pending selection stopped recovery", err)
				}
				var gotState, reason string
				if err = f.store.DB().QueryRow(`SELECT state,reason FROM review_selections WHERE id=?`, receipt.ID).Scan(&gotState, &reason); err != nil {
					t.Fatal(err)
				}
				if gotState != "held" || reason != "local_read_unavailable" || reader.calls.Load() != 0 {
					t.Fatal("damaged selection was not isolated before source I/O", gotState, reason)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
					t.Fatal("damaged selection created child")
				}
			})
		}
	}
}
