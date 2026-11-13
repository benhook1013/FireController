package reviewexec

import (
	"context"
	"strings"
	"testing"
)

func TestCLIDraftProjectionRetainsUnusedAllowanceHistory(t *testing.T) {
	for _, corruption := range []string{"valid", "malformed", "blob", "oversized"} {
		t.Run(corruption, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "draft-corrupt-projection", "cli", 2, 1))
			in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 3, Limit: 5, FullCapacityInMS: 12000, RollingWindowMS: 3600000, ChangedFiles: 2}
			if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
				t.Fatal(err)
			}
			body := any("{")
			if corruption == "blob" {
				body = []byte("{}")
			}
			if corruption == "oversized" {
				body = strings.Repeat("x", 8193)
			}
			if corruption != "valid" {
				admissionSQL(t, f, "UPDATE review_cli_preflight SET body_json=? WHERE attempt_id=?", body, a.ID)
			}
			before := snapshotTable(t, f, "review_cli_preflight")
			held, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", cliDraftWait, "")
			if err != nil {
				t.Fatal("optional allowance rolled back draft detachment", err)
			}
			o, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, a.OperationID)
			if err != nil || o.State != "queued" || o.Reason != cliDraftWait || o.AttemptID != "" {
				t.Fatal(o, err)
			}
			if held.State != "held" || attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
				t.Fatal("unused attempt retained capacity", held)
			}
			_, projected := projectedAttempt(t, f, held)
			assertProjectedTimes(t, f, held, projected, true)
			if after := snapshotTable(t, f, "review_cli_preflight"); after != before {
				t.Fatal("projection changed retained allowance evidence")
			}
			original, historical, err := historicalCLIDraftOperation(t.Context(), f.store.DB(), o, held)
			if err != nil || !historical {
				t.Fatal("draft history lost its original binding", err)
			}
			record, err := readCLIAllowanceRecord(t.Context(), f.store.DB(), original, held)
			if corruption == "valid" {
				if err != nil || record.Observation.Remaining != in.Remaining {
					t.Fatal("history lost the original positive observation", err)
				}
			} else if !storedAllowanceUnavailable(err) {
				t.Fatal("history projection granted usable allowance", err)
			}
			assertCLIWaitNoCredit(t, f)
		})
	}
}

func TestCLIDraftProjectionPreservesOperationalFailures(t *testing.T) {
	for _, failure := range []string{"storage", "context"} {
		t.Run(failure, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "draft-failed-projection", "cli", 2, 1))
			before := attemptState(t, f, a)
			ctx := t.Context()
			if failure == "storage" {
				admissionSQL(t, f, "ALTER TABLE review_cli_preflight RENAME TO unavailable_preflight")
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "held", cliDraftWait, ""); err == nil || storedAllowanceUnavailable(err) {
				t.Fatal("operational failure suppressed or relabeled", err)
			}
			if after := attemptState(t, f, a); after != before {
				t.Fatal("failed transaction changed attempt/reservation")
			}
		})
	}
}
