package reviewexec

import (
	"testing"
)

func TestHostedSnapshotRetainsUnusedSchedulingPreflight(t *testing.T) {
	for _, reason := range []string{"pull_scheduling_paused", "automation_authorization_changed", "automatic_scheduling_unavailable"} {
		t.Run(reason, func(t *testing.T) {
			f, first, _, input := hostedAllowanceFixture(t)
			if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, first.ID, first.Revision, input); err != nil {
				t.Fatal(err)
			}
			if _, err := f.e.Advance(t.Context(), attemptProject, first.ID, first.Revision, "held", reason, ""); err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				tables := []string{"review_hosted_preflight", "review_hosted_allowance_queries", "review_operations", "review_operation_waits"}
				before := make([]string, len(tables))
				for i, table := range tables {
					before[i] = snapshotTable(t, f, table)
				}
				tx, err := f.store.DB().BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := ValidateAllowanceSnapshot(t.Context(), tx, 10); err != nil {
					t.Fatalf("valid detached Hosted preflight refused: %v", err)
				}
				for i, table := range tables {
					if after := snapshotTableThrough(t, tx, table); before[i] != after {
						t.Fatalf("snapshot wrote to %s inside inspection", table)
					}
				}
			}
			check()
			next := f.start(t, attemptProject, Receipt{ID: first.OperationID})
			if next.ID == "" || next.ID == first.ID {
				t.Fatal("new attempt not independently attached", next)
			}
			check()
			o, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, first.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if o.AttemptID != next.ID {
				t.Fatal("snapshot rebound the current operation", o.AttemptID)
			}
			detached, err := readAttempt(t.Context(), f.store.DB(), attemptProject, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readHostedAllowanceRecord(t.Context(), f.store.DB(), o, detached); err == nil {
				t.Fatal("live allowance lookup accepted detached scheduling history")
			}
		})
	}
}
